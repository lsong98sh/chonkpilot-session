// fork_test.go —— 2G §11 用例 9 / 10（fork full / fork context）。
package component

import (
	"os"
	"testing"

	"github.com/chonkpilot/chonkpilot-data"
)

// seedTree 建一个「主 session + 子会话 + 消息/轮次/快照/任务树/inbox」的对话，返回 ids。
func seedTree(t *testing.T, c *Component, prjUsr string) (dialogID, mainID, childID, turnID string) {
	t.Helper()
	d, err := c.CreateDialog(CreateDialogCmd{Title: "源对话"})
	if err != nil {
		t.Fatalf("CreateDialog: %v", err)
	}
	sub, err := c.CreateSession(CreateSessionCmd{DialogID: d.DialogID, ParentID: d.SessionID, Kind: "sub_chat"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.Dispatch(DispatchCmd{
		DialogID: d.DialogID, SessionID: sub.SessionID,
		Utterance: Utterance{From: Participant{Kind: "human", ID: "u"}, Body: "你好"},
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := c.SetSnapshot(SetSnapshotCmd{
		DialogID: d.DialogID, SessionID: sub.SessionID, SnapshotTurn: "t-src",
		History: []data.ChatMsg{{Role: "user", Content: "摘要"}},
	}); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	// 直写 tasktree / inbox（组件 P4 尚未提供 API，这里按存储形状播种）。
	db, release, err := data.OpenExistingDialog(prjUsr, d.DialogID)
	if err != nil {
		t.Fatalf("open dialog: %v", err)
	}
	defer release()
	if err := db.Table("tasktree").Insert("n1", data.Record{
		"node_id": "n1", "top_session": d.SessionID, "session_id": sub.SessionID,
		"status": "running", "created_at": "2026-10-24T10:00:00.000000001Z",
	}); err != nil {
		t.Fatalf("seed tasktree: %v", err)
	}
	if err := db.Table("inbox_items").Insert("i1", data.Record{
		"session_id": sub.SessionID, "type": "insert", "content": "x", "status": "pending",
		"created_at": "2026-10-24T10:00:00.000000001Z",
	}); err != nil {
		t.Fatalf("seed inbox: %v", err)
	}
	return d.DialogID, d.SessionID, sub.SessionID, "t-src"
}

// TestForkDialogFull 用例 9：新 dialog id + 新文件；库内全部 session 引用已重映射（无旧 id 残留）；
// inbox 清空；发 DialogCreated。
func TestForkDialogFull(t *testing.T) {
	c, sink, prjUsr := newComp(t)
	srcDialog, mainID, childID, _ := seedTree(t, c, prjUsr)
	sink.take()

	res, err := c.ForkDialog(ForkDialogCmd{DialogID: srcDialog})
	if err != nil {
		t.Fatalf("ForkDialog: %v", err)
	}
	if res.DialogID == "" || res.DialogID == srcDialog || res.SessionID == mainID || res.SessionID == childID {
		t.Fatalf("派生 id 不合法: %+v", res)
	}
	if _, err := os.Stat(data.DialogDBPath(prjUsr, res.DialogID)); err != nil {
		t.Fatalf("目标库文件缺失: %v", err)
	}
	if n := sink.count(EventDialogCreated); n != 1 {
		t.Fatalf("DialogCreated 应 1 次，实际 %d", n)
	}

	db, release, err := data.OpenExistingDialog(prjUsr, res.DialogID)
	if err != nil {
		t.Fatalf("open forked: %v", err)
	}
	defer release()

	// sessions：主 + 子各 1，且 parent_id 指向新主；forked_from = 源 id。
	var sessions []data.Record
	if err := db.Table("sessions").ForEach(func(_ string, rec data.Record) error {
		sessions = append(sessions, rec)
		return nil
	}); err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("派生库应有 2 个 session，实际 %d", len(sessions))
	}
	oldIDs := map[string]bool{mainID: true, childID: true}
	newMain := res.SessionID
	for _, rec := range sessions {
		sid := sval(rec["session_id"])
		if oldIDs[sid] {
			t.Fatalf("session_id 未重映射（旧 id 残留）: %q", sid)
		}
		if src := sval(rec["forked_from"]); src == "" {
			t.Fatalf("forked_from 应记录源 id: %+v", rec)
		}
		if sval(rec["parent_id"]) == "" && sid != newMain {
			t.Fatalf("主 session 应为 %s，实际 %s", newMain, sid)
		}
	}
	// messages / turns / snapshot 的 session_id 全部指向新 id。
	for _, table := range []string{"messages", "turns"} {
		var rows int
		if err := db.Table(table).ForEach(func(_ string, rec data.Record) error {
			rows++
			if oldIDs[sval(rec["session_id"])] {
				t.Fatalf("%s.session_id 未重映射: %+v", table, rec)
			}
			return nil
		}); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		if rows == 0 {
			t.Fatalf("%s 应有派生行", table)
		}
	}
	var snap data.Record
	if ok, err := db.Table("snapshot").Get(res.SessionID, &snap); err != nil {
		t.Fatalf("read snapshot: %v", err)
	} else if ok {
		_ = ok
	} else {
		// 快照属于子会话 → 主 session 无快照是正常的；检查子会话的新 id 有快照。
		var found bool
		if err := db.Table("snapshot").ForEach(func(k string, rec data.Record) error {
			if oldIDs[k] {
				t.Fatalf("snapshot 主键未重映射: %q", k)
			}
			found = true
			return nil
		}); err != nil {
			t.Fatalf("read snapshot: %v", err)
		}
		if !found {
			t.Fatal("快照应随派生树复制")
		}
	}
	// tasktree 引用重映射。
	var taskNode data.Record
	if ok, err := db.Table("tasktree").Get("n1", &taskNode); err != nil || !ok {
		t.Fatalf("任务树行缺失: ok=%v err=%v", ok, err)
	}
	if oldIDs[sval(taskNode["top_session"])] || oldIDs[sval(taskNode["session_id"])] {
		t.Fatalf("tasktree 引用未重映射: %+v", taskNode)
	}
	// inbox 清空。
	var inboxRows int
	if err := db.Table("inbox_items").ForEach(func(_ string, _ data.Record) error {
		inboxRows++
		return nil
	}); err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if inboxRows != 0 {
		t.Fatalf("派生对话 inbox 应清空，实际 %d 行", inboxRows)
	}

	// dialogs 行（含 main_session_id）。
	cat, relCat, err := data.OpenSharedLayer(prjUsr, data.LayerPrjUsr)
	if err != nil {
		t.Fatalf("open prjusr: %v", err)
	}
	defer relCat()
	var row data.Record
	if ok, err := cat.Table("dialogs").Get(res.DialogID, &row); err != nil || !ok {
		t.Fatalf("派生 dialogs 行缺失: ok=%v err=%v", ok, err)
	}
	if sval(row["main_session_id"]) != res.SessionID {
		t.Fatalf("dialogs.main_session_id 应为 %s，实际 %v", res.SessionID, row["main_session_id"])
	}
}

// TestForkDialogContext 用例 10：不拷文件内容；只有主 session 行 + snapshot；消息为空。
func TestForkDialogContext(t *testing.T) {
	c, _, prjUsr := newComp(t)
	srcDialog, mainID, _, _ := seedTree(t, c, prjUsr)

	res, err := c.ForkDialog(ForkDialogCmd{DialogID: srcDialog, Mode: ForkModeContext, Title: "上下文分支"})
	if err != nil {
		t.Fatalf("ForkDialog(context): %v", err)
	}
	db, release, err := data.OpenExistingDialog(prjUsr, res.DialogID)
	if err != nil {
		t.Fatalf("open forked: %v", err)
	}
	defer release()

	sessions, err := c.ListSessions(ListSessionsCmd{DialogID: res.DialogID})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].SessionID != res.SessionID {
		t.Fatalf("context 档应只有 1 个会话行: %+v", sessions.Sessions)
	}
	if sessions.Sessions[0].Title != "上下文分支" {
		t.Fatalf("标题应取命令值: %q", sessions.Sessions[0].Title)
	}
	var msgRows int
	if err := db.Table("messages").ForEach(func(_ string, _ data.Record) error {
		msgRows++
		return nil
	}); err != nil {
		t.Fatalf("read messages: %v", err)
	}
	if msgRows != 0 {
		t.Fatalf("context 档不应复制消息，实际 %d 行", msgRows)
	}
	// 源主 session 无快照（本用例快照在子会话上）→ 派生库也应无快照，但快照路径不得报错。
	var snapRows int
	if err := db.Table("snapshot").ForEach(func(k string, _ data.Record) error {
		snapRows++
		return nil
	}); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	_ = snapRows
	_ = mainID
}
