package component

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chonkpilot/chonkpilot-session/store"
)

// scripted 是可控 Dispatcher（记录输入、返回固定决策）。
type scripted struct {
	mu        sync.Mutex
	decisions []Decision
	calls     []Context
	utts      []Utterance
}

func (s *scripted) Dispatch(ctx Context, u Utterance) []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, ctx)
	s.utts = append(s.utts, u)
	return s.decisions
}

// newCompD 建带 Dispatcher 的组件（其余同 newComp）。
func newCompD(t *testing.T, d Dispatcher) (*Component, *nopSink, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	sink := &nopSink{}
	c, err := New(Options{
		Store:      st,
		Sink:       sink,
		Clock:      &fakeClock{t: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)},
		Dispatcher: d,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, sink, st
}

// TestSinkReentrantDispatch 用例 14：sink 内再 Dispatch 不 panic、不死锁（提交后才回调）。
func TestSinkReentrantDispatch(t *testing.T) {
	st := store.NewMemory()
	var (
		c        *Component
		dlg      CreateDialogRes
		nested   atomic.Bool
		deadlock = make(chan struct{}, 1)
	)
	sink := SinkFunc(func(ev Event) {
		if ev.Kind != EventMessageAppended || !nested.CompareAndSwap(false, true) {
			return
		}
		go func() {
			// 用 goroutine 派发：若组件在回调期间持锁，这里会死锁 → 通道超时暴露。
			_, _ = c.Dispatch(DispatchCmd{
				DialogID: dlg.DialogID, SessionID: dlg.SessionID,
				Utterance: Utterance{From: Participant{Kind: "llm", ID: "l1"}, Body: "回调内再发", ClientMsgID: "reentrant-1"},
			})
			deadlock <- struct{}{}
		}()
	})
	var err error
	c, err = New(Options{Store: st, Sink: sink, Clock: &fakeClock{t: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dlg, err = c.CreateDialog(CreateDialogCmd{Title: "重入"})
	if err != nil {
		t.Fatalf("CreateDialog: %v", err)
	}
	if _, err := c.Dispatch(DispatchCmd{
		DialogID: dlg.DialogID, SessionID: dlg.SessionID,
		Utterance: Utterance{From: Participant{Kind: "human", ID: "u1"}, Body: "外层"},
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	select {
	case <-deadlock:
	case <-time.After(5 * time.Second):
		t.Fatal("sink 内再 Dispatch 发生死锁")
	}
	db := openDlg(t, st, dlg.DialogID)
	if msgs := messagesOf(t, db, dlg.SessionID); len(msgs) != 2 {
		t.Fatalf("重入后应有 2 条消息，实际 %d", len(msgs))
	}
}

// openDlg 打开对话库（测试读断言用）。
func openDlg(t *testing.T, st *store.Memory, dialogID string) store.DB {
	t.Helper()
	db, release, err := st.OpenDialog(dialogID)
	if err != nil {
		t.Fatalf("OpenDialog: %v", err)
	}
	t.Cleanup(release)
	return db
}

// messagesOf 读会话消息（created_at 升序）。
func messagesOf(t *testing.T, db store.DB, sessionID string) []store.Record {
	t.Helper()
	recs, _, err := db.Table("messages").Query(store.Query{
		Where: store.Record{"session_id": sessionID}, OrderBy: "created_at",
	})
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	return recs
}

// TestDispatchSingleChat 用例 4：human 说 → 落库 + 自动开轮 + 事件序；llm 回 + Finish → 关轮。
func TestDispatchSingleChat(t *testing.T) {
	d := &scripted{decisions: []Decision{{Kind: DecisionPersist}, {Kind: DecisionDeliverLocal}}}
	c, sink, st := newCompD(t, d)
	dlg, err := c.CreateDialog(CreateDialogCmd{Title: "单聊"})
	if err != nil {
		t.Fatalf("CreateDialog: %v", err)
	}
	sink.take()

	res, err := c.Dispatch(DispatchCmd{
		DialogID: dlg.DialogID, SessionID: dlg.SessionID,
		Utterance: Utterance{From: Participant{Kind: "human", ID: "u1"}, Body: "你好", ClientMsgID: "c1"},
	})
	if err != nil {
		t.Fatalf("Dispatch(human): %v", err)
	}
	if res.Replayed || res.TurnID == "" || res.MessageID == "" || len(res.Decisions) != 2 {
		t.Fatalf("human 结果不符: %+v", res)
	}
	evs := sink.take()
	if len(evs) != 2 || evs[0].Kind != EventTurnOpened || evs[1].Kind != EventMessageAppended {
		t.Fatalf("事件序应为 TurnOpened→MessageAppended: %+v", evs)
	}
	if len(d.calls) != 1 || d.calls[0].Session.Kind != "main" || d.calls[0].Self != "u1" {
		t.Fatalf("Dispatcher 上下文不符: %+v", d.calls)
	}

	db := openDlg(t, st, dlg.DialogID)
	msgs := messagesOf(t, db, dlg.SessionID)
	if len(msgs) != 1 || sval(msgs[0]["role"]) != "user" || sval(msgs[0]["client_msg_id"]) != "c1" {
		t.Fatalf("消息落库不符: %+v", msgs)
	}
	var trec store.Record
	if ok, _ := db.Table("turns").Get(res.TurnID, &trec); !ok || sval(trec["status"]) != TurnRunning {
		t.Fatalf("应自动开 running turn: %+v", trec)
	}

	// llm 回 + Finish → 关轮
	d.mu.Lock()
	d.decisions = []Decision{{Kind: DecisionPersist}, {Kind: DecisionFinish}}
	d.mu.Unlock()
	res2, err := c.Dispatch(DispatchCmd{
		DialogID: dlg.DialogID, SessionID: dlg.SessionID,
		Utterance: Utterance{From: Participant{Kind: "llm", ID: "llm1"}, Body: "在"},
	})
	if err != nil {
		t.Fatalf("Dispatch(llm): %v", err)
	}
	if res2.TurnID != res.TurnID {
		t.Fatalf("应复用同一 open turn: %s vs %s", res2.TurnID, res.TurnID)
	}
	if ok, _ := db.Table("turns").Get(res.TurnID, &trec); !ok || sval(trec["status"]) != TurnCompleted {
		t.Fatalf("Finish 决策应关轮 completed: %+v", trec)
	}
	evs = sink.take()
	if len(evs) != 2 || evs[0].Kind != EventMessageAppended || evs[1].Kind != EventTurnClosed {
		t.Fatalf("第二轮事件序不符: %+v", evs)
	}
}

// TestDispatchIdempotent 用例 5：同 client_msg_id 二次 → Replayed、无新事件、无新消息。
func TestDispatchIdempotent(t *testing.T) {
	d := &scripted{decisions: []Decision{{Kind: DecisionPersist}}}
	c, sink, st := newCompD(t, d)
	dlg, _ := c.CreateDialog(CreateDialogCmd{Title: "幂等"})
	sink.take()
	cmd := DispatchCmd{
		DialogID: dlg.DialogID, SessionID: dlg.SessionID,
		Utterance: Utterance{From: Participant{Kind: "human", ID: "u1"}, Body: "一次", ClientMsgID: "dup-1"},
	}
	first, err := c.Dispatch(cmd)
	if err != nil {
		t.Fatalf("Dispatch #1: %v", err)
	}
	sink.take()
	second, err := c.Dispatch(cmd)
	if err != nil {
		t.Fatalf("Dispatch #2: %v", err)
	}
	if !second.Replayed || second.MessageID != first.MessageID || second.TurnID != first.TurnID {
		t.Fatalf("二次应幂等回放: %+v", second)
	}
	if evs := sink.take(); len(evs) != 0 {
		t.Fatalf("幂等命中不应发事件: %+v", evs)
	}
	if len(d.calls) != 1 {
		t.Fatalf("幂等命中不应再调 Dispatcher: %d", len(d.calls))
	}
	db := openDlg(t, st, dlg.DialogID)
	if msgs := messagesOf(t, db, dlg.SessionID); len(msgs) != 1 {
		t.Fatalf("应只有 1 条消息: %d", len(msgs))
	}
}

// TestDispatchHumanForcePersist 用例 6：dispatcher 只返 Finish 也照样落库。
func TestDispatchHumanForcePersist(t *testing.T) {
	d := &scripted{decisions: []Decision{{Kind: DecisionFinish}}}
	c, _, st := newCompD(t, d)
	dlg, _ := c.CreateDialog(CreateDialogCmd{Title: "强制"})
	res, err := c.Dispatch(DispatchCmd{
		DialogID: dlg.DialogID, SessionID: dlg.SessionID,
		Utterance: Utterance{From: Participant{Kind: "human", ID: "u1"}, Body: "必须落库"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !hasDecision(res.Decisions, DecisionPersist) {
		t.Fatalf("human 应被强制补 Persist: %+v", res.Decisions)
	}
	db := openDlg(t, st, dlg.DialogID)
	if msgs := messagesOf(t, db, dlg.SessionID); len(msgs) != 1 {
		t.Fatalf("human 消息应落库: %d", len(msgs))
	}
}

// TestTurnOpenClose 用例：OpenTurn 幂等 / CloseTurn 终态守卫 / 非法状态。
func TestTurnOpenClose(t *testing.T) {
	c, sink, _ := newCompD(t, PersistOnly{})
	dlg, _ := c.CreateDialog(CreateDialogCmd{Title: "轮次"})
	sink.take()

	first, err := c.OpenTurn(OpenTurnCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID})
	if err != nil || !first.Opened || first.TurnID == "" {
		t.Fatalf("OpenTurn #1: %+v %v", first, err)
	}
	second, err := c.OpenTurn(OpenTurnCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID})
	if err != nil || second.Opened || second.TurnID != first.TurnID {
		t.Fatalf("OpenTurn 应复用已开轮: %+v %v", second, err)
	}
	if n := sink.count(EventTurnOpened); n != 1 {
		t.Fatalf("TurnOpened 应 1 次，实际 %d", n)
	}

	if err := c.CloseTurn(CloseTurnCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID, TurnID: first.TurnID}); err != nil {
		t.Fatalf("CloseTurn: %v", err)
	}
	if err := c.CloseTurn(CloseTurnCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID, TurnID: first.TurnID}); CodeOf(err) != CodeTurnClosed {
		t.Fatalf("重复关轮应 E_TURN_CLOSED: %v", err)
	}
	if err := c.CloseTurn(CloseTurnCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID, TurnID: "ghost"}); CodeOf(err) != CodeTurnNotFound {
		t.Fatalf("未知轮次应 E_TURN_NOT_FOUND: %v", err)
	}
	t3, _ := c.OpenTurn(OpenTurnCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID})
	if err := c.CloseTurn(CloseTurnCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID, TurnID: t3.TurnID, Status: "running"}); CodeOf(err) != CodeInvalid {
		t.Fatalf("非法状态应 E_INVALID: %v", err)
	}
}

// TestFinishAbortEvents：Finish / Abort 各发 TurnClosed + Finished。
func TestFinishAbortEvents(t *testing.T) {
	c, sink, _ := newCompD(t, PersistOnly{})
	dlg, _ := c.CreateDialog(CreateDialogCmd{Title: "收口"})
	sink.take()

	t1, _ := c.OpenTurn(OpenTurnCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID})
	sink.take() // 丢弃 DialogCreated / TurnOpened，只看 Finish 的两条
	if err := c.Finish(FinishCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID, TurnID: t1.TurnID, Reason: "done"}); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	evs := sink.take()
	if len(evs) != 2 || evs[0].Kind != EventTurnClosed || evs[0].Status != TurnCompleted ||
		evs[1].Kind != EventFinished || evs[1].Reason != "done" {
		t.Fatalf("Finish 事件不符: %+v", evs)
	}

	t2, _ := c.OpenTurn(OpenTurnCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID})
	sink.take()
	if err := c.Abort(AbortCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID, TurnID: t2.TurnID}); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	evs = sink.take()
	if len(evs) != 2 || evs[0].Status != TurnAborted || evs[1].Kind != EventFinished || evs[1].Reason != "aborted" {
		t.Fatalf("Abort 事件不符: %+v", evs)
	}
}

// TestSnapshotRoundTrip：写 / 读一致（含 ChatMsg 结构）+ SnapshotSet 事件 + 未知会话报错。
func TestSnapshotRoundTrip(t *testing.T) {
	c, sink, _ := newCompD(t, PersistOnly{})
	dlg, _ := c.CreateDialog(CreateDialogCmd{Title: "快照"})
	sink.take()

	hist := []store.ChatMsg{
		{Role: "user", Content: "第一句"},
		{Role: "assistant", Content: "回复", Reasoning: "think"},
	}
	if err := c.SetSnapshot(SetSnapshotCmd{
		DialogID: dlg.DialogID, SessionID: dlg.SessionID, SnapshotTurn: "t1", History: hist,
	}); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	if n := sink.count(EventSnapshotSet); n != 1 {
		t.Fatalf("SnapshotSet 应 1 次，实际 %d", n)
	}
	got, err := c.GetSnapshot(GetSnapshotCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID})
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if !got.Found || got.SnapshotTurn != "t1" || len(got.History) != 2 ||
		got.History[1].Reasoning != "think" {
		t.Fatalf("快照往返不符: %+v", got)
	}
	if _, err := c.GetSnapshot(GetSnapshotCmd{DialogID: dlg.DialogID, SessionID: "ghost"}); err != nil {
		t.Fatalf("无快照应返回 Found=false 而非错误: %v", err)
	}
	if err := c.SetSnapshot(SetSnapshotCmd{DialogID: dlg.DialogID, SessionID: "ghost"}); CodeOf(err) != CodeSessionNotFound {
		t.Fatalf("未知会话应 E_SESSION_NOT_FOUND: %v", err)
	}
}

// TestDispatchAddressing：缺 dialog / session 不存在的错误码。
func TestDispatchAddressing(t *testing.T) {
	c, _, _ := newCompD(t, PersistOnly{})
	dlg, _ := c.CreateDialog(CreateDialogCmd{Title: "寻址"})
	if _, err := c.Dispatch(DispatchCmd{SessionID: dlg.SessionID, Utterance: Utterance{Body: "x"}}); CodeOf(err) != CodeDialogRequired {
		t.Fatalf("缺 dialog 应 E_DIALOG_REQUIRED: %v", err)
	}
	if _, err := c.Dispatch(DispatchCmd{DialogID: dlg.DialogID, SessionID: "ghost", Utterance: Utterance{Body: "x"}}); CodeOf(err) != CodeSessionNotFound {
		t.Fatalf("未知会话应 E_SESSION_NOT_FOUND: %v", err)
	}
	if _, err := c.Dispatch(DispatchCmd{DialogID: dlg.DialogID, SessionID: dlg.SessionID, Utterance: Utterance{Body: ""}}); CodeOf(err) != CodeInvalid {
		t.Fatalf("空正文应 E_INVALID: %v", err)
	}
}
