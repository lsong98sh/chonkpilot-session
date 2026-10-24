// Package component 是 **Session 组件的存储侧实现**（SESSION-COMP-1 P1）。
//
// 与父包 `chonkpilot-session` 的分工：
//
//	父包（纯 lib）  上下文组装 / 压缩 / 任务树运行态 —— 不读 MQ / DB
//	本包（存储侧）  对话（一树一库）的读写与事件 —— **只依赖 store 端口**，不碰 MQ、不碰具体数据库
//
// 持久化（2026-10-24，Persist Provider）：具体「怎么切库、怎么落盘」由注入的 `store.Store`
// 决定（bbolt 一树一库 / 单库 / Oracle / 内存均可）；组件只决定表名、主键与行字段。
//
// 布局（一树一库的 bbolt 实现）：prjusr `dialogs` 目录 + `<prjusr>/dialogs/dialog-<hex(dialog_id)>.db`
// （一库 = 一棵会话树：主 session 1 行 + 子会话 N 行 + messages / turns / tasktree / inbox / snapshot）。
//
// 寻址（2G §4.1）：所有会话类调用必须给 `DialogID`；组件**不做** session → dialog 全局反查。
package component

import (
	"errors"
	"fmt"
	"time"

	"github.com/chonkpilot/chonkpilot-session/store"
)

// Clock 是时间源（测试注入；nil = 系统时钟）。
type Clock interface{ Now() time.Time }

// Options 是组件构造参数。
type Options struct {
	// Store 是持久化端口（必填；实现决定切分与落盘方式）。
	Store store.Store
	// Sink 是事件出口（nil = 不广播）。
	Sink Sink
	// Clock 时间源（nil = time.Now）。
	Clock Clock
	// Dispatcher 是投递策略（nil = PersistOnly；P2 提供 Relay / Fanout / Record）。
	Dispatcher Dispatcher
}

// Component 是 Session 组件的存储侧门面（对话 / 会话树 / 轮次 / 消息 / 快照）。
type Component struct {
	store      store.Store
	sink       Sink
	clock      Clock
	dispatcher Dispatcher
}

// New 构造组件（不打开任何库；调用按需短开）。
func New(opts Options) (*Component, error) {
	if opts.Store == nil {
		return nil, errors.New("component: store required（Persist Provider 未注入）")
	}
	return &Component{
		store:      opts.Store,
		sink:       opts.Sink,
		clock:      opts.Clock,
		dispatcher: opts.Dispatcher,
	}, nil
}

// Now 返回当前时间（UTC 纳秒固定 9 位，数据面统一口径）。
func (c *Component) nowStr() string {
	t := time.Now()
	if c.clock != nil {
		t = c.clock.Now()
	}
	return t.UTC().Format(store.RFC3339FixedNano)
}

// emit 同步投递事件（Sink 约定：只入队、不 panic、不长阻塞）。
func (c *Component) emit(ev Event) {
	if c == nil || c.sink == nil {
		return
	}
	defer func() { _ = recover() }() // 事件不得影响提交结果（2G §4.3）
	c.sink.Emit(ev)
}

// catalog 短开 prjusr 主库（配对 release）。
func (c *Component) catalog() (store.DB, func(), error) {
	return c.store.Catalog()
}

// openDialog 短开对话库（配对 release）。
func (c *Component) openDialog(dialogID string) (store.DB, func(), error) {
	if dialogID == "" {
		return nil, nil, Err(CodeDialogRequired, "dialog_id required")
	}
	return c.store.OpenDialog(dialogID)
}

// DialogRow 是 prjusr `dialogs` 表的一行（列表展示用副本）。
type DialogRow struct {
	DialogID string `json:"dialog_id"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	// MainSessionID 主 session id（列表免开库；P3-4a 起随行落库）。
	MainSessionID string `json:"main_session_id,omitempty"`
	UpdatedAt     string `json:"updated_at"`
}

// SessionRow 是对话库内 `sessions` 表的一行。
type SessionRow struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	ParentID  string `json:"parent_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// getDialogRow 读 dialogs 行（ok=false = 不存在）。
func getDialogRow(cat store.DB, dialogID string) (store.Record, bool, error) {
	var rec store.Record
	ok, err := cat.Table("dialogs").Get(dialogID, &rec)
	return rec, ok, err
}

// putDialogRow 写 / 覆盖 dialogs 行（`file` 命名由 Store 决定）。
func (c *Component) putDialogRow(cat store.DB, row DialogRow) error {
	if row.DialogID == "" {
		return Err(CodeDialogRequired, "dialog_id required")
	}
	rec := store.Record{
		"dialog_id":  row.DialogID,
		"file":       c.store.DialogFileName(row.DialogID),
		"title":      row.Title,
		"kind":       row.Kind,
		"updated_at": row.UpdatedAt,
	}
	if row.MainSessionID != "" {
		rec["main_session_id"] = row.MainSessionID
	}
	return cat.Table("dialogs").Upsert(row.DialogID, rec)
}

// mainSession 返回对话库内的主 session 行（parent_id == ""；无 → ok=false）。
func mainSession(sdb store.DB) (store.Record, bool, error) {
	var best store.Record
	err := sdb.Table("sessions").ForEach(func(_ string, rec store.Record) error {
		if sval(rec["parent_id"]) != "" {
			return nil
		}
		if best == nil {
			best = rec
		}
		return nil
	})
	return best, best != nil, err
}

// sessionByID 读库内某 session 行。
func sessionByID(sdb store.DB, sessionID string) (store.Record, bool, error) {
	var rec store.Record
	ok, err := sdb.Table("sessions").Get(sessionID, &rec)
	return rec, ok, err
}

// sval 把 Record 值转字符串（nil → ""）。
func sval(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// isNotFound 判定存储层的「行不存在」。
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
