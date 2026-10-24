// turn.go —— 轮次（Turn）与收口：OpenTurn / CloseTurn / Finish / Abort（2G §4 / §8）。
//
// 语义：
//   - 一次一来一回；`Dispatch` 无 open turn 时自动开（见 dispatch.go）；
//   - 关轮后不允许再补（`E_TURN_CLOSED`）；不存在 → `E_TURN_NOT_FOUND`；
//   - `Finish` = 关轮(completed) + `Finished` 事件；`Abort` = 关轮(aborted) + `Finished`；
//   - 启动清理（running → interrupted）沿用数据面 `data-session-cleanup-stale`，本组件不重复实现。
package component

import (
	"github.com/lsong98sh/chonkpilot-session/store"
)

// 轮次状态白名单（与既有数据面一致）。
const (
	TurnRunning     = "running"
	TurnCompleted   = "completed"
	TurnAborted     = "aborted"
	TurnInterrupted = "interrupted"
)

// OpenTurnCmd 显式开轮（`TurnID` 空 = 生成）。
type OpenTurnCmd struct {
	DialogID  string
	SessionID string
	TurnID    string
}

// OpenTurnRes 是开轮结果（`Opened=false` = 命中已开轮，未新建、未发事件）。
type OpenTurnRes struct {
	TurnID string `json:"turn_id"`
	Opened bool   `json:"opened"`
}

// CloseTurnCmd 显式关轮（`Status` 空 → completed）。
type CloseTurnCmd struct {
	DialogID     string
	SessionID    string
	TurnID       string
	Status       string
	FinishReason string
}

// FinishCmd 收口（关轮 completed + Finished 事件）。
type FinishCmd struct {
	DialogID  string
	SessionID string
	TurnID    string
	Reason    string
}

// AbortCmd 中止（关轮 aborted + Finished 事件）。
type AbortCmd struct {
	DialogID  string
	SessionID string
	TurnID    string
	Reason    string
}

// OpenTurn 显式开轮：该会话已有 running turn → 原样返回（Opened=false，幂等）。
func (c *Component) OpenTurn(cmd OpenTurnCmd) (OpenTurnRes, error) {
	if cmd.DialogID == "" {
		return OpenTurnRes{}, Err(CodeDialogRequired, "dialog_id required")
	}
	if cmd.SessionID == "" {
		return OpenTurnRes{}, Err(CodeSessionNotFound, "session_id required")
	}
	sdb, release, err := c.openDialog(cmd.DialogID)
	if err != nil {
		return OpenTurnRes{}, c.mapOpenErr(err, cmd.DialogID)
	}
	defer release()
	if _, ok, err := sessionByID(sdb, cmd.SessionID); err != nil {
		return OpenTurnRes{}, Err(CodeBackend, "读 sessions: %v", err)
	} else if !ok {
		return OpenTurnRes{}, Err(CodeSessionNotFound, "session %s 不在对话 %s", cmd.SessionID, cmd.DialogID)
	}
	if rec, ok, err := findOpenTurn(sdb, cmd.SessionID); err != nil {
		return OpenTurnRes{}, Err(CodeBackend, "读 turns: %v", err)
	} else if ok {
		return OpenTurnRes{TurnID: sval(rec["turn_id"])}, nil
	}
	turnID := cmd.TurnID
	if turnID == "" {
		turnID = c.mustID()
	}
	now := c.nowStr()
	if err := sdb.Table("turns").Insert(turnID, store.Record{
		"turn_id": turnID, "session_id": cmd.SessionID, "status": TurnRunning,
		"created_at": now, "updated_at": now,
	}); err != nil {
		return OpenTurnRes{}, Err(CodeBackend, "写 turn: %v", err)
	}
	c.emit(Event{Kind: EventTurnOpened, DialogID: cmd.DialogID, SessionID: cmd.SessionID, TurnID: turnID})
	return OpenTurnRes{TurnID: turnID, Opened: true}, nil
}

// CloseTurn 关轮（已关 → E_TURN_CLOSED；不存在 → E_TURN_NOT_FOUND）。
func (c *Component) CloseTurn(cmd CloseTurnCmd) error {
	status := firstNonEmpty(cmd.Status, TurnCompleted)
	if !validTerminalStatus(status) {
		return Err(CodeInvalid, "turn status 非法: %q（completed|aborted|interrupted）", status)
	}
	return c.closeTurn(cmd.DialogID, cmd.SessionID, cmd.TurnID, status, cmd.FinishReason)
}

// Finish 收口：关轮 completed + Finished。
func (c *Component) Finish(cmd FinishCmd) error {
	if err := c.closeTurn(cmd.DialogID, cmd.SessionID, cmd.TurnID, TurnCompleted, firstNonEmpty(cmd.Reason, "finished")); err != nil {
		return err
	}
	c.emit(Event{
		Kind: EventFinished, DialogID: cmd.DialogID, SessionID: cmd.SessionID, TurnID: cmd.TurnID,
		Status: TurnCompleted, Reason: firstNonEmpty(cmd.Reason, "finished"),
	})
	return nil
}

// Abort 中止：关轮 aborted + Finished。
func (c *Component) Abort(cmd AbortCmd) error {
	if err := c.closeTurn(cmd.DialogID, cmd.SessionID, cmd.TurnID, TurnAborted, firstNonEmpty(cmd.Reason, "aborted")); err != nil {
		return err
	}
	c.emit(Event{
		Kind: EventFinished, DialogID: cmd.DialogID, SessionID: cmd.SessionID, TurnID: cmd.TurnID,
		Status: TurnAborted, Reason: firstNonEmpty(cmd.Reason, "aborted"),
	})
	return nil
}

// ── 内部 ───────────────────────────────────────────────────────

// closeTurn 关轮（幂等边界：已终态 → E_TURN_CLOSED）。
func (c *Component) closeTurn(dialogID, sessionID, turnID, status, reason string) error {
	if dialogID == "" {
		return Err(CodeDialogRequired, "dialog_id required")
	}
	if turnID == "" {
		return Err(CodeTurnNotFound, "turn_id required")
	}
	if !validTerminalStatus(status) {
		return Err(CodeInvalid, "turn status 非法: %q", status)
	}
	sdb, release, err := c.openDialog(dialogID)
	if err != nil {
		return c.mapOpenErr(err, dialogID)
	}
	defer release()
	var trec store.Record
	has, err := sdb.Table("turns").Get(turnID, &trec)
	if err != nil {
		return Err(CodeBackend, "读 turns: %v", err)
	}
	if !has {
		return Err(CodeTurnNotFound, "turn 不存在: %s", turnID)
	}
	if sessionID != "" && sval(trec["session_id"]) != sessionID {
		return Err(CodeTurnNotFound, "turn %s 不属于会话 %s", turnID, sessionID)
	}
	if sval(trec["status"]) != TurnRunning {
		return Err(CodeTurnClosed, "turn %s 已关闭（status=%s）", turnID, sval(trec["status"]))
	}
	now := c.nowStr()
	if err := sdb.Update(func(tx store.Tx) error {
		cur := store.Record{}
		if _, err := tx.Get("turns", turnID, &cur); err != nil {
			return err
		}
		if len(cur) == 0 {
			return store.ErrNotFound
		}
		cur["status"] = status
		cur["finish_reason"] = reason
		cur["updated_at"] = now
		if err := tx.Put("turns", turnID, cur); err != nil {
			return err
		}
		sid := sval(cur["session_id"])
		if sid == "" {
			sid = sessionID
		}
		if sid != "" {
			return touchSession(tx, sid, now)
		}
		return nil
	}); err != nil {
		return Err(CodeBackend, "关轮: %v", err)
	}
	c.emit(Event{
		Kind: EventTurnClosed, DialogID: dialogID, SessionID: firstNonEmpty(sessionID, sval(trec["session_id"])),
		TurnID: turnID, Status: status, FinishReason: reason,
	})
	return nil
}

// findOpenTurn 找该会话最新的 running turn（无 → ok=false）。
func findOpenTurn(sdb store.DB, sessionID string) (store.Record, bool, error) {
	var best store.Record
	bestCreated := ""
	err := sdb.Table("turns").ForEach(func(_ string, rec store.Record) error {
		if sval(rec["session_id"]) != sessionID || sval(rec["status"]) != TurnRunning {
			return nil
		}
		c := sval(rec["created_at"])
		if best == nil || c > bestCreated {
			best, bestCreated = rec, c
		}
		return nil
	})
	return best, best != nil, err
}

// touchTurn 事务内刷新 turn.updated_at（不存在 → 忽略，交由调用方判据）。
func touchTurn(tx store.Tx, turnID, now string) error {
	rec := store.Record{}
	ok, err := tx.Get("turns", turnID, &rec)
	if err != nil || !ok {
		return err
	}
	rec["updated_at"] = now
	return tx.Put("turns", turnID, rec)
}

// touchSession 事务内刷新 session.updated_at（行不存在 → 忽略）。
func touchSession(tx store.Tx, sessionID, now string) error {
	rec := store.Record{}
	ok, err := tx.Get("sessions", sessionID, &rec)
	if err != nil || !ok {
		return err
	}
	rec["updated_at"] = now
	return tx.Put("sessions", sessionID, rec)
}

// validTerminalStatus 判定终态白名单（running 不可由关轮写入）。
func validTerminalStatus(status string) bool {
	switch status {
	case TurnCompleted, TurnAborted, TurnInterrupted:
		return true
	}
	return false
}
