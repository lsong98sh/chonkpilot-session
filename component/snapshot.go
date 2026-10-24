// snapshot.go —— 上下文快照（2G §3.2 `snapshot` 表：仅 `history` / `snapshot_turn`）。
//
// 存储形状与既有数据面**逐字节一致**（`history` = JSON `[]store.ChatMsg`），
// 便于与历史行互通。
package component

import (
	"encoding/json"

	"github.com/lsong98sh/chonkpilot-session/store"
)

// SetSnapshotCmd 写会话快照（`SessionID` 必填；`History` 为空 = 清空快照）。
type SetSnapshotCmd struct {
	DialogID     string
	SessionID    string
	SnapshotTurn string
	History      []store.ChatMsg
}

// GetSnapshotCmd 读会话快照。
type GetSnapshotCmd struct {
	DialogID  string
	SessionID string
}

// Snapshot 是快照读取结果（`Found=false` = 尚无快照）。
type Snapshot struct {
	Found        bool            `json:"found"`
	SnapshotTurn string          `json:"snapshot_turn"`
	History      []store.ChatMsg `json:"history"`
}

// SetSnapshot 写快照（同事务刷新 session.updated_at；提交后发 SnapshotSet）。
func (c *Component) SetSnapshot(cmd SetSnapshotCmd) error {
	if cmd.DialogID == "" {
		return Err(CodeDialogRequired, "dialog_id required")
	}
	if cmd.SessionID == "" {
		return Err(CodeSessionNotFound, "session_id required")
	}
	raw, err := json.Marshal(cmd.History)
	if err != nil {
		return Err(CodeInvalid, "history 序列化失败: %v", err)
	}
	sdb, release, err := c.openDialog(cmd.DialogID)
	if err != nil {
		return c.mapOpenErr(err, cmd.DialogID)
	}
	defer release()
	if _, ok, err := sessionByID(sdb, cmd.SessionID); err != nil {
		return Err(CodeBackend, "读 sessions: %v", err)
	} else if !ok {
		return Err(CodeSessionNotFound, "session %s 不在对话 %s", cmd.SessionID, cmd.DialogID)
	}
	now := c.nowStr()
	if err := sdb.Update(func(tx store.Tx) error {
		if err := tx.Put("snapshot", cmd.SessionID, store.Record{
			"session_id":    cmd.SessionID,
			"history":       string(raw),
			"snapshot_turn": cmd.SnapshotTurn,
			"updated_at":    now,
		}); err != nil {
			return err
		}
		return touchSession(tx, cmd.SessionID, now)
	}); err != nil {
		return Err(CodeBackend, "写快照: %v", err)
	}
	c.emit(Event{
		Kind: EventSnapshotSet, DialogID: cmd.DialogID, SessionID: cmd.SessionID, SnapshotTurn: cmd.SnapshotTurn,
	})
	return nil
}

// GetSnapshot 读快照（无 → Found=false；坏 JSON → E_BACKEND，不静默当空）。
func (c *Component) GetSnapshot(cmd GetSnapshotCmd) (Snapshot, error) {
	if cmd.DialogID == "" {
		return Snapshot{}, Err(CodeDialogRequired, "dialog_id required")
	}
	if cmd.SessionID == "" {
		return Snapshot{}, Err(CodeSessionNotFound, "session_id required")
	}
	sdb, release, err := c.openDialog(cmd.DialogID)
	if err != nil {
		return Snapshot{}, c.mapOpenErr(err, cmd.DialogID)
	}
	defer release()
	var rec store.Record
	ok, err := sdb.Table("snapshot").Get(cmd.SessionID, &rec)
	if err != nil {
		return Snapshot{}, Err(CodeBackend, "读快照: %v", err)
	}
	if !ok {
		return Snapshot{}, nil
	}
	out := Snapshot{Found: true, SnapshotTurn: sval(rec["snapshot_turn"])}
	if raw := sval(rec["history"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &out.History); err != nil {
			return Snapshot{}, Err(CodeBackend, "快照 history 解析失败: %v", err)
		}
	}
	return out, nil
}
