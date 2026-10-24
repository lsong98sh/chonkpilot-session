package component

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chonkpilot/chonkpilot-data"
)

// fakeClock 是可推进的时钟（断言 updated_at 排序）。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

// nopSink 记录事件（线程安全）。
type nopSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *nopSink) Emit(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *nopSink) take() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.events
	s.events = nil
	return out
}

func (s *nopSink) count(kind EventKind) int {
	n := 0
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// newComp 建临时 prjusr + 组件（带 fake clock 与事件 sink）。
func newComp(t *testing.T) (*Component, *nopSink, string) {
	t.Helper()
	dir := t.TempDir()
	prjUsr := filepath.Join(dir, "prjusr", "chonkpilot.db")
	sink := &nopSink{}
	c, err := New(Options{PrjUsrPath: prjUsr, Sink: sink, Clock: &fakeClock{t: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 预热 prjusr（建表 + 迁移），配对释放以允许 TempDir 清理。
	db, release, err := data.OpenSharedLayer(prjUsr, data.LayerPrjUsr)
	if err != nil {
		t.Fatalf("预热 prjusr: %v", err)
	}
	_ = db
	release()
	return c, sink, prjUsr
}

// TestCreateDialog 用例 1（创建）：dialog_id ≠ session_id、文件/目录行/主 session 齐备、事件一次。
func TestCreateDialog(t *testing.T) {
	c, sink, prjUsr := newComp(t)
	res, err := c.CreateDialog(CreateDialogCmd{Title: "对话一"})
	if err != nil {
		t.Fatalf("CreateDialog: %v", err)
	}
	if res.DialogID == "" || res.SessionID == "" || res.DialogID == res.SessionID {
		t.Fatalf("id 不合法: %+v", res)
	}
	if _, err := os.Stat(data.DialogDBPath(prjUsr, res.DialogID)); err != nil {
		t.Fatalf("对话库文件缺失: %v", err)
	}
	cat, release, err := data.OpenSharedLayer(prjUsr, data.LayerPrjUsr)
	if err != nil {
		t.Fatalf("open prjusr: %v", err)
	}
	var row data.Record
	if ok, err := cat.Table("dialogs").Get(res.DialogID, &row); err != nil || !ok {
		t.Fatalf("dialogs 行缺失: ok=%v err=%v", ok, err)
	}
	for _, k := range []string{"dialog_id", "file", "title", "kind", "updated_at"} {
		if _, has := row[k]; !has {
			t.Fatalf("dialogs 行缺字段 %s: %+v", k, row)
		}
	}
	release()

	sessions, err := c.ListSessions(ListSessionsCmd{DialogID: res.DialogID})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].ParentID != "" || sessions.Sessions[0].SessionID != res.SessionID {
		t.Fatalf("应恰有 1 个主 session: %+v", sessions.Sessions)
	}
	if n := sink.count(EventDialogCreated); n != 1 {
		t.Fatalf("DialogCreated 事件应 1 次，实际 %d", n)
	}
}

// TestEnsureDialogIdempotent 用例 1（幂等）：重复 Ensure 不新建、不重复发事件。
func TestEnsureDialogIdempotent(t *testing.T) {
	c, sink, _ := newComp(t)
	id, _ := data.NewDialogID()
	first, err := c.EnsureDialog(EnsureDialogCmd{DialogID: id, Title: "幂等"})
	if err != nil {
		t.Fatalf("Ensure #1: %v", err)
	}
	if !first.Created {
		t.Fatal("首次 Ensure 应 Created=true")
	}
	second, err := c.EnsureDialog(EnsureDialogCmd{DialogID: id, Title: "改名不该生效"})
	if err != nil {
		t.Fatalf("Ensure #2: %v", err)
	}
	if second.Created || second.SessionID != first.SessionID {
		t.Fatalf("重复 Ensure 应命中既有: %+v", second)
	}
	if n := sink.count(EventDialogCreated); n != 1 {
		t.Fatalf("DialogCreated 应恰 1 次，实际 %d", n)
	}
	dialogs, err := c.ListDialogs()
	if err != nil || len(dialogs) != 1 {
		t.Fatalf("应只有 1 个对话: %v %+v", err, dialogs)
	}
	if dialogs[0].Title != "幂等" {
		t.Fatalf("重复 Ensure 不应改标题: %+v", dialogs[0])
	}
}

// TestEnsureDialogHalfCreated 半创建态（库在、无主 session）→ 补齐且只补一次。
func TestEnsureDialogHalfCreated(t *testing.T) {
	c, _, prjUsr := newComp(t)
	id, _ := data.NewDialogID()
	// 只建空库（无 session 行、无目录行）
	db, release, err := data.OpenDialog(prjUsr, id)
	if err != nil {
		t.Fatalf("OpenDialog: %v", err)
	}
	_ = db
	release()

	first, err := c.EnsureDialog(EnsureDialogCmd{DialogID: id, MainSessionID: "main-1", Title: "半创建"})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if first.SessionID != "main-1" || !first.Created {
		t.Fatalf("应按给定 id 补主 session: %+v", first)
	}
	second, err := c.EnsureDialog(EnsureDialogCmd{DialogID: id, MainSessionID: "main-2"})
	if err != nil {
		t.Fatalf("Ensure #2: %v", err)
	}
	if second.SessionID != "main-1" {
		t.Fatalf("补齐后应复用既有主 session: %+v", second)
	}
	s, _ := c.ListSessions(ListSessionsCmd{DialogID: id})
	if len(s.Sessions) != 1 {
		t.Fatalf("主 session 应恰 1 个: %+v", s.Sessions)
	}
}

// TestParticipantLimit 用例 3：同一 session 两个 llm → E_PARTICIPANT_LIMIT。
func TestParticipantLimit(t *testing.T) {
	c, _, _ := newComp(t)
	_, err := c.CreateDialog(CreateDialogCmd{
		Title: "越界",
		Participants: []Participant{
			{Kind: "llm", LLM: "a"},
			{Kind: "llm", LLM: "b"},
		},
	})
	if CodeOf(err) != CodeParticipantLimit {
		t.Fatalf("应 E_PARTICIPANT_LIMIT，实际 %v（code=%q）", err, CodeOf(err))
	}
}

// TestCreateSessionInvariants 用例 2 + 11：只能建子会话；父须存在；寻址缺 dialog → E_DIALOG_REQUIRED。
func TestCreateSessionInvariants(t *testing.T) {
	c, sink, _ := newComp(t)
	res, err := c.CreateDialog(CreateDialogCmd{Title: "树"})
	if err != nil {
		t.Fatalf("CreateDialog: %v", err)
	}
	if _, err := c.CreateSession(CreateSessionCmd{DialogID: res.DialogID}); CodeOf(err) != CodeMainLimit {
		t.Fatalf("根会话应 E_MAIN_LIMIT: %v", err)
	}
	if _, err := c.CreateSession(CreateSessionCmd{DialogID: res.DialogID, ParentID: "nope"}); CodeOf(err) != CodeSessionNotFound {
		t.Fatalf("父不存在应 E_SESSION_NOT_FOUND: %v", err)
	}
	child, err := c.CreateSession(CreateSessionCmd{DialogID: res.DialogID, ParentID: res.SessionID, Kind: "sub_chat", Title: "子"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if child.SessionID == "" || child.SessionID == res.SessionID {
		t.Fatalf("子会话 id 不合法: %+v", child)
	}
	s, _ := c.ListSessions(ListSessionsCmd{DialogID: res.DialogID})
	if len(s.Sessions) != 2 {
		t.Fatalf("应有主 + 子 2 行: %+v", s.Sessions)
	}
	main := 0
	for _, row := range s.Sessions {
		if row.ParentID == "" {
			main++
		}
	}
	if main != 1 {
		t.Fatalf("主 session 应恰 1 个: %+v", s.Sessions)
	}
	if n := sink.count(EventSessionCreated); n != 1 {
		t.Fatalf("SessionCreated 应 1 次，实际 %d", n)
	}
	if _, err := c.ListSessions(ListSessionsCmd{}); CodeOf(err) != CodeDialogRequired {
		t.Fatalf("缺 dialog 应 E_DIALOG_REQUIRED: %v", err)
	}
}

// TestRename 用例 8：主 session 改名同步 dialogs；子会话改名不同步；事件区分。
func TestRename(t *testing.T) {
	c, sink, _ := newComp(t)
	res, err := c.CreateDialog(CreateDialogCmd{Title: "原标题"})
	if err != nil {
		t.Fatalf("CreateDialog: %v", err)
	}
	child, err := c.CreateSession(CreateSessionCmd{DialogID: res.DialogID, ParentID: res.SessionID, Title: "子原"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sink.take()

	if err := c.RenameDialog(RenameDialogCmd{DialogID: res.DialogID, Title: "新标题"}); err != nil {
		t.Fatalf("RenameDialog: %v", err)
	}
	if got := dialogTitle(t, c, res.DialogID); got != "新标题" {
		t.Fatalf("dialogs.title 未同步: %q", got)
	}
	evs := sink.take()
	if len(evs) != 1 || evs[0].Kind != EventTitleChanged || !evs[0].IsDialogTitle {
		t.Fatalf("RenameDialog 事件不符: %+v", evs)
	}

	if err := c.RenameSession(RenameSessionCmd{DialogID: res.DialogID, SessionID: child.SessionID, Title: "子新"}); err != nil {
		t.Fatalf("RenameSession(child): %v", err)
	}
	if got := dialogTitle(t, c, res.DialogID); got != "新标题" {
		t.Fatalf("子会话改名不应动 dialogs.title: %q", got)
	}
	evs = sink.take()
	if len(evs) != 1 || evs[0].IsDialogTitle {
		t.Fatalf("子会话改名事件不符: %+v", evs)
	}

	if err := c.RenameSession(RenameSessionCmd{DialogID: res.DialogID, SessionID: res.SessionID, Title: "主新"}); err != nil {
		t.Fatalf("RenameSession(main): %v", err)
	}
	if got := dialogTitle(t, c, res.DialogID); got != "主新" {
		t.Fatalf("主 session 改名应同步 dialogs.title: %q", got)
	}
	evs = sink.take()
	if len(evs) != 1 || !evs[0].IsDialogTitle {
		t.Fatalf("主 session 改名事件应 IsDialogTitle=true: %+v", evs)
	}

	if err := c.RenameSession(RenameSessionCmd{DialogID: res.DialogID, SessionID: "ghost", Title: "x"}); CodeOf(err) != CodeSessionNotFound {
		t.Fatalf("未知 session 应 E_SESSION_NOT_FOUND: %v", err)
	}
	if err := c.RenameDialog(RenameDialogCmd{DialogID: res.DialogID, Title: strings.Repeat("长", 129)}); CodeOf(err) != CodeInvalid {
		t.Fatalf("超长标题应 E_INVALID: %v", err)
	}
}

// TestDeleteIdempotent：删除幂等（文件与行）、事件只发一次。
func TestDeleteIdempotent(t *testing.T) {
	c, sink, prjUsr := newComp(t)
	res, err := c.CreateDialog(CreateDialogCmd{Title: "待删"})
	if err != nil {
		t.Fatalf("CreateDialog: %v", err)
	}
	path := data.DialogDBPath(prjUsr, res.DialogID)
	if err := c.DeleteDialog(DeleteDialogCmd{DialogID: res.DialogID}); err != nil {
		t.Fatalf("DeleteDialog: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("库文件应已删除: %v", err)
	}
	if n := sink.count(EventDialogDeleted); n != 1 {
		t.Fatalf("DialogDeleted 应 1 次，实际 %d", n)
	}
	if err := c.DeleteDialog(DeleteDialogCmd{DialogID: res.DialogID}); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
	if n := sink.count(EventDialogDeleted); n != 1 {
		t.Fatalf("重复删除不应再发事件，实际 %d", n)
	}
	if err := c.DeleteDialog(DeleteDialogCmd{}); CodeOf(err) != CodeDialogRequired {
		t.Fatalf("缺 dialog 应 E_DIALOG_REQUIRED: %v", err)
	}
}

// TestListDialogsOrder：按 updated_at 降序。
func TestListDialogsOrder(t *testing.T) {
	c, _, _ := newComp(t)
	a, err := c.CreateDialog(CreateDialogCmd{Title: "A"})
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	b, err := c.CreateDialog(CreateDialogCmd{Title: "B"})
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	list, err := c.ListDialogs()
	if err != nil {
		t.Fatalf("ListDialogs: %v", err)
	}
	if len(list) != 2 || list[0].DialogID != b.DialogID || list[1].DialogID != a.DialogID {
		t.Fatalf("应按 updated_at 降序: %+v", list)
	}
}

// dialogTitle 读 dialogs.title。
func dialogTitle(t *testing.T, c *Component, dialogID string) string {
	t.Helper()
	list, err := c.ListDialogs()
	if err != nil {
		t.Fatalf("ListDialogs: %v", err)
	}
	for _, row := range list {
		if row.DialogID == dialogID {
			return row.Title
		}
	}
	return ""
}
