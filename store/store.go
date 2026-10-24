// Package store 是会话组件的**持久化端口**（Persist Provider）。
//
// 边界（2026-10-24 定，41 D-69 ②）：
//
//	chonkpilot-session   组件只依赖本包的接口与值类型 —— 不 import chonkpilot-data，也不碰 bbolt
//	宿主 / 适配器        由宿主决定「怎么切、怎么存」（一树一库 bbolt / 单库 / Oracle / 内存），
//	                    实现 Store 后注入 `component.Options{Store: ...}`
//
// 组件仍然决定**表名 / 主键 / 行字段**（会话域 schema 属领域契约）；Store 只负责
// 「打开哪本库、行怎么落盘、事务怎么提交」。
package store

import "errors"

// Record 是表行（主键 → JSON 值）。字段值为 `any`，由持久化实现决定编码。
type Record map[string]any

// Query 是通用查询条件（等值过滤 / 排序 / 分页 / 游标）。
type Query struct {
	Where     map[string]any // 等值过滤（支持点路径）
	OrderBy   string         // 排序字段（点路径）
	OrderDesc bool
	Limit     int    // 0 = 不限
	Offset    int    // 普通分页偏移（与 Cursor 互斥）
	Cursor    string // 连续滚动游标（上页 next_cursor；优先于 Offset）
}

// KeyField 是 Query / Get 返回记录时附加的主键字段名。
const KeyField = "_key"

// RFC3339FixedNano 是固定 9 位纳秒 RFC3339 时间格式（同秒内字典序 = 时间序）。
const RFC3339FixedNano = "2006-01-02T15:04:05.000000000Z07:00"

// 通用错误哨兵（实现须原样返回，供调用方 errors.Is 判定）。
var (
	ErrNotFound = errors.New("store: key not found")
	ErrExists   = errors.New("store: key already exists")
)

// Fork 档位。
const (
	ForkModeFull    = "full"    // 整棵树一起派生（复制库 + 全量重映射）
	ForkModeContext = "context" // 只派生上下文（空库 + 主会话 + 快照）
)

// fork 错误哨兵（组件映射为自己的错误码）。
var (
	ErrForkDialogNotFound = errors.New("store: fork 对话不存在")
	ErrForkNoMainSession  = errors.New("store: fork 源对话无主会话")
	ErrForkTargetExists   = errors.New("store: fork 目标对话库已存在")
	ErrForkNoSessions     = errors.New("store: fork 源对话无会话行")
)

// ForkResult 是派生结果（新 dialog_id + 新主 session_id / 标题 / 类型）。
type ForkResult struct {
	DialogID  string
	SessionID string
	Title     string
	Kind      string
}

// Store 是**库级**持久化能力（切分策略由实现决定）。
type Store interface {
	// Catalog 打开对话目录库（prjusr 层；dialogs 表所在）。调用方必须配对 release。
	Catalog() (DB, func(), error)
	// OpenDialog 打开对话库（不存在则创建）。调用方必须配对 release。
	OpenDialog(dialogID string) (DB, func(), error)
	// ExistingDialog 打开**已存在**的对话库（缺失 → 错误，不静默新建）。
	ExistingDialog(dialogID string) (DB, func(), error)
	// CloneDialog 把源对话库整体复制为新对话库（一致性快照；目标已存在 → ErrForkTargetExists）。
	CloneDialog(srcDialogID, dstDialogID string) error
	// DeleteDialog 删除对话库（幂等）。
	DeleteDialog(dialogID string) error
	// DialogFileName 返回目录行里的 `file` 值（命名规则由实现决定）。
	DialogFileName(dialogID string) string
	// NewID 生成新 id（dialog / session / turn 共用；实现通常为 UUID v4）。
	NewID() (string, error)
}

// DB 是一本库的句柄（短开：调用方配对 release）。
type DB interface {
	Path() string
	Table(name string) Table
	// Update 在**单个写事务**内执行 fn（跨表原子；fn 返回错误 → 整体回滚）。
	Update(fn func(tx Tx) error) error
}

// Table 是表 DAO。
type Table interface {
	// Get 按主键读（不存在 → ok=false）；out 非 nil 时回填记录（含 KeyField）。
	Get(key string, out *Record) (bool, error)
	// Insert 插入（已存在 → ErrExists）。
	Insert(key string, rec Record) error
	// Upsert 写入（不存在插入、存在覆盖）。
	Upsert(key string, rec Record) error
	// UpdateIn 单事务「读-改-写」：fn 返回 nil = 放弃写入；不存在且返回非 nil = 插入。
	UpdateIn(key string, fn func(rec Record) Record) error
	// Delete 删除（不存在 → ErrNotFound）。
	Delete(key string) error
	// ForEach 单事务遍历全部行（k = 主键，rec 含 KeyField）。
	ForEach(fn func(key string, rec Record) error) error
	// Query 过滤 / 排序 / 分页（返回记录带 KeyField）。
	Query(q Query) ([]Record, string, error)
}

// Tx 是单次写事务句柄（跨表原子写）。
type Tx interface {
	Get(table, key string, out *Record) (bool, error)
	Insert(table, key string, rec Record) error
	Put(table, key string, rec Record) error
	Delete(table, key string) error
	ForEach(table string, fn func(key string, rec Record) error) error
}

// ── 消息 / 快照契约类型（与既有数据面 JSON 逐字段一致）────────────────

// ChatMsg 是 chat 消息（对齐 OpenAI 协议；快照格式）。
type ChatMsg struct {
	Role       string         `json:"role"`
	Kind       string         `json:"kind,omitempty"`
	Content    string         `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	Reasoning  string         `json:"reasoning,omitempty"`
	Meta       map[string]any `json:"_meta,omitempty"`
}

// ToolCall 是 LLM 请求的工具调用（快照格式）。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Zones 是三段上下文的轮数划分（升序：摘要区 < 简化区 < 完整区）。
type Zones struct {
	FullTurns  int
	BriefTurns int
}
