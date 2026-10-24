// fork.go —— 对话派生（2G §5，SESSION-COMP-1 P4）。
//
// **实现唯一 = 本组件**（2026-10-24，Persist Provider 改造）：数据面门面 `SessionFork` 也调
// `Component.ForkDialog`，存储细节全部经 `store.Store`（复制库 / 事务 / 行读写）。
//
// 机制（full）：`Store.CloneDialog` 复制库（一致性快照）→ 单事务重映射 session 引用 →
// 写 dialogs 行。context：不复制库，只写主 session 行 + 源主 session 的 snapshot。
//
// 原子性：跨库非原子 —— 库先落、dialogs 行后写；崩在中间 → 孤儿库由启动对账收敛。
package component

import (
	"errors"

	"github.com/chonkpilot/chonkpilot-session/store"
)

// Fork 档位（调用方不必区分两层）。
const (
	ForkModeFull    = store.ForkModeFull
	ForkModeContext = store.ForkModeContext
)

// ForkDialogCmd 派生一个对话（`DialogID` = 源对话，必填）。
type ForkDialogCmd struct {
	DialogID string
	Mode     string // "" → full
	Title    string // "" → 源主 session 标题
}

// ForkRes 是派生结果（新 dialog_id + 新主 session_id）。
type ForkRes struct {
	DialogID  string `json:"dialog_id"`
	SessionID string `json:"session_id"`
}

// ForkDialog 派生对话（见文件头机制说明）。
func (c *Component) ForkDialog(cmd ForkDialogCmd) (ForkRes, error) {
	if cmd.DialogID == "" {
		return ForkRes{}, Err(CodeDialogRequired, "dialog_id required")
	}
	mode := firstNonEmpty(cmd.Mode, ForkModeFull)
	if mode != ForkModeFull && mode != ForkModeContext {
		return ForkRes{}, Err(CodeInvalid, "fork mode 非法: %q（full|context）", mode)
	}

	cat, release, err := c.catalog()
	if err != nil {
		return ForkRes{}, Err(CodeBackend, "打开目录库: %v", err)
	}
	srcRow, exists, err := getDialogRow(cat, cmd.DialogID)
	release()
	if err != nil {
		return ForkRes{}, Err(CodeBackend, "读 dialogs 行: %v", err)
	}
	if !exists {
		return ForkRes{}, Err(CodeDialogNotFound, "%v: %s", store.ErrForkDialogNotFound, cmd.DialogID)
	}

	srcDB, relSrc, err := c.store.ExistingDialog(cmd.DialogID)
	if err != nil {
		return ForkRes{}, Err(CodeBackend, "打开源对话库: %v", err)
	}
	srcMain, hasMain, err := mainSession(srcDB)
	if err != nil {
		relSrc()
		return ForkRes{}, Err(CodeBackend, "读 sessions: %v", err)
	}
	if !hasMain {
		relSrc()
		return ForkRes{}, Err(CodeSessionNotFound, "%v: %s", store.ErrForkNoMainSession, cmd.DialogID)
	}
	srcMainID := sval(srcMain["session_id"])
	titleOut := firstNonEmpty(cmd.Title, sval(srcMain["title"]), sval(srcRow["title"]))
	kind := firstNonEmpty(sval(srcMain["kind"]), sval(srcRow["kind"]), "main")

	newID, err := c.store.NewID()
	if err != nil {
		relSrc()
		return ForkRes{}, Err(CodeBackend, "生成 dialog_id: %v", err)
	}

	var newMainID string
	if mode == ForkModeFull {
		if err := c.store.CloneDialog(cmd.DialogID, newID); err != nil {
			relSrc()
			return ForkRes{}, c.mapForkErr(err)
		}
		newMainID, err = c.forkFull(newID)
	} else {
		newMainID, err = c.forkContext(srcDB, newID, srcMainID, titleOut, kind)
	}
	relSrc()
	if err != nil {
		_ = c.store.DeleteDialog(newID) // 清半成品（幂等）
		return ForkRes{}, c.mapForkErr(err)
	}

	cat2, relCat, err := c.catalog()
	if err != nil {
		_ = c.store.DeleteDialog(newID)
		return ForkRes{}, Err(CodeBackend, "打开目录库: %v", err)
	}
	rowErr := c.putDialogRow(cat2, DialogRow{
		DialogID: newID, Title: titleOut, Kind: kind, MainSessionID: newMainID, UpdatedAt: c.nowStr(),
	})
	relCat()
	if rowErr != nil {
		_ = c.store.DeleteDialog(newID)
		return ForkRes{}, Err(CodeBackend, "写 dialogs 行: %v", rowErr)
	}
	c.emit(Event{
		Kind: EventDialogCreated, DialogID: newID, SessionID: newMainID,
		Title: titleOut, SessionKind: kind,
	})
	return ForkRes{DialogID: newID, SessionID: newMainID}, nil
}

// forkFull 在已复制的库内做单事务 id 重映射；返回新主 session id。
func (c *Component) forkFull(newID string) (string, error) {
	tdb, release, err := c.store.OpenDialog(newID)
	if err != nil {
		return "", err
	}
	defer release()

	newMainID := ""
	err = tdb.Update(func(tx store.Tx) error {
		// sessions：先收集（勿边遍历边改），再删旧键、写新键。
		var sessions []store.Record
		if err := tx.ForEach("sessions", func(_ string, rec store.Record) error {
			sessions = append(sessions, rec)
			return nil
		}); err != nil {
			return err
		}
		if len(sessions) == 0 {
			return store.ErrForkNoSessions
		}
		mapping := make(map[string]string, len(sessions))
		for _, rec := range sessions {
			old := sval(rec["session_id"])
			if old == "" {
				continue
			}
			nid, err := c.store.NewID()
			if err != nil {
				return err
			}
			mapping[old] = nid
		}
		for _, rec := range sessions {
			old := sval(rec["session_id"])
			if old == "" {
				continue
			}
			if err := tx.Delete("sessions", old); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		for _, rec := range sessions {
			old := sval(rec["session_id"])
			if old == "" {
				continue
			}
			nr := cloneRecord(rec)
			nr["session_id"] = mapping[old]
			parent := sval(rec["parent_id"])
			if parent == "" {
				newMainID = mapping[old]
			} else if np, ok := mapping[parent]; ok {
				nr["parent_id"] = np
			}
			nr["forked_from"] = old
			if err := tx.Insert("sessions", mapping[old], nr); err != nil {
				return err
			}
		}
		// messages / turns：主键不变，只换 session_id。
		for _, table := range []string{"messages", "turns"} {
			var rows []forkRow
			if err := tx.ForEach(table, func(k string, rec store.Record) error {
				rows = append(rows, forkRow{k, rec})
				return nil
			}); err != nil {
				return err
			}
			for _, row := range rows {
				nsid, ok := mapping[sval(row.rec["session_id"])]
				if !ok {
					continue
				}
				nr := cloneRecord(row.rec)
				nr["session_id"] = nsid
				if err := tx.Put(table, row.key, nr); err != nil {
					return err
				}
			}
		}
		// snapshot：主键 = session_id → 删旧键、插新键。
		var snaps []forkRow
		if err := tx.ForEach("snapshot", func(k string, rec store.Record) error {
			snaps = append(snaps, forkRow{k, rec})
			return nil
		}); err != nil {
			return err
		}
		for _, s := range snaps {
			nsid, ok := mapping[s.key]
			if !ok {
				continue
			}
			if err := tx.Delete("snapshot", s.key); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			nr := cloneRecord(s.rec)
			nr["session_id"] = nsid
			if err := tx.Insert("snapshot", nsid, nr); err != nil {
				return err
			}
		}
		// tasktree：top_session / session_id 指向新树。
		var taskRows []forkRow
		if err := tx.ForEach("tasktree", func(k string, rec store.Record) error {
			taskRows = append(taskRows, forkRow{k, rec})
			return nil
		}); err != nil {
			return err
		}
		for _, row := range taskRows {
			nr := cloneRecord(row.rec)
			changed := false
			if nsid, ok := mapping[sval(row.rec["top_session"])]; ok {
				nr["top_session"] = nsid
				changed = true
			}
			if nsid, ok := mapping[sval(row.rec["session_id"])]; ok {
				nr["session_id"] = nsid
				changed = true
			}
			if !changed {
				continue
			}
			if err := tx.Put("tasktree", row.key, nr); err != nil {
				return err
			}
		}
		// inbox_items：派生对话不带未消费插话（full 档也清空）。
		var inboxKeys []string
		if err := tx.ForEach("inbox_items", func(k string, _ store.Record) error {
			inboxKeys = append(inboxKeys, k)
			return nil
		}); err != nil {
			return err
		}
		for _, k := range inboxKeys {
			if err := tx.Delete("inbox_items", k); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newMainID, nil
}

// forkContext 空库 + 主 session 行 + 源主 session 的 snapshot（不拷消息）。
func (c *Component) forkContext(srcDB store.DB, newID, srcMainID, title, kind string) (string, error) {
	tdb, release, err := c.store.OpenDialog(newID)
	if err != nil {
		return "", err
	}
	defer release()

	newMainID, err := c.store.NewID()
	if err != nil {
		return "", err
	}
	now := c.nowStr()

	// 源快照（无 / 坏行 → 不复制，不阻断派生）。
	var srcSnap store.Record
	hasSnap, snapErr := srcDB.Table("snapshot").Get(srcMainID, &srcSnap)
	if snapErr != nil {
		hasSnap = false
	}
	if err := tdb.Update(func(tx store.Tx) error {
		if err := tx.Insert("sessions", newMainID, store.Record{
			"session_id": newMainID, "title": title, "kind": kind, "parent_id": "",
			"forked_from": srcMainID, "created_at": now, "updated_at": now,
		}); err != nil {
			return err
		}
		if !hasSnap {
			return nil
		}
		nr := cloneRecord(srcSnap)
		nr["session_id"] = newMainID
		return tx.Insert("snapshot", newMainID, nr)
	}); err != nil {
		return "", err
	}
	return newMainID, nil
}

// forkRow 是「主键 + 行」对（先收集后改的中间形态）。
type forkRow struct {
	key string
	rec store.Record
}

// cloneRecord 复制一行（剥离 `_key`）。
func cloneRecord(rec store.Record) store.Record {
	out := make(store.Record, len(rec))
	for k, v := range rec {
		if k == store.KeyField {
			continue
		}
		out[k] = v
	}
	return out
}

// mapForkErr 把存储层 fork 错误映射为组件错误码。
func (c *Component) mapForkErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrForkDialogNotFound):
		return Err(CodeDialogNotFound, "%v", err)
	case errors.Is(err, store.ErrForkNoMainSession), errors.Is(err, store.ErrForkNoSessions):
		return Err(CodeSessionNotFound, "%v", err)
	default:
		return Err(CodeBackend, "派生对话: %v", err)
	}
}
