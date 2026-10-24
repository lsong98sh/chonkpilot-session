// 端口层：本组件对宿主的**依赖倒置入口**（接口在消费方声明、由宿主实现）。
//
// 边界：本包不读 MQ / DB / 配置文件、不调 LLM——所有取数与副作用都经下述端口由宿主
// （`src/lib/llm/server`）提供。命名与宿主现有取数点一一对应：
//
//	HistorySource  ← 会话历史 + turn_tokens + 本轮已落库消息（宿主 `BuildContextTokens` / `LoadMessages`）
//	LayerSource    ← 系统提示词五层拼装 + 占位符替换（宿主 `turn.go`）
//	GuideSource    ← 记忆 / 资产两条指引（宿主 `memory_guide.go`，宿主负责单轮缓存）
//	PolicySource   ← 三段边界配置（宿主 `keep_full_max_turns` / `keep_full_max_tokens` /
//	                 `compress_token_threshold` 三个键；**不新增配置键**）
//	ToolSource     ← 本轮下发工具面（宿主 `llmTools`；仅用于 schema token 估算）
//	SnapshotStore  ← 会话快照读写（宿主 `SnapshotGet` / `SnapshotSet`）
//	Summarizer     ← 摘要生成（宿主 `llmOnceMessages`）
//	TaskStore      ← 任务树数据面读 / 收敛（宿主任务层 `chonkpilot-task`；单写者口径）
//	TaskEmitter    ← 任务树事件广播（宿主 `tasks.started/updated/done`）
//	TaskExecSink   ← 任务执行侧取消（宿主 gateway 执行池）
//	InstanceWorkDir ← work_dir 解析（宿主 `instance.Manager`）
package session

import "context"

// HistorySource 提供组装取数：会话历史（含 turn_tokens 预存值）与当前进行中 turn 的已落库消息。
// 读失败由宿主按"空历史"保守处置（与既有语义一致：读不到不猜）。
type HistorySource interface {
	Load(ctx context.Context) (History, error)
}

// LayerSource 提供本轮系统提示词（五层拼装 + 占位符替换后的最终文本）。
type LayerSource interface {
	SystemPrompt(ctx context.Context) string
}

// GuideSource 提供两条指引正文（记忆 / 资产；空串 = 不注入）。
// 宿主负责"单轮只读一次"的缓存语义（组件每轮工具迭代都会组装）。
type GuideSource interface {
	Memory(ctx context.Context) string
	Asset(ctx context.Context) string
}

// PolicySource 提供三段边界配置值（原样返回，不做默认值折算——默认值归宿主配置读取层）。
type PolicySource interface {
	KeepFullTurns(ctx context.Context) int
	KeepFullTokens(ctx context.Context) int
	BriefBudget(ctx context.Context) int
}

// ToolSource 提供本轮下发工具面（仅用于工具 schema 的 token 估算）。
type ToolSource interface {
	Tools(ctx context.Context) []ToolSchema
}

// Snapshot 是一份会话快照（`sessions.history` + `snapshot_turn`）。
// Found=false = 会话尚无快照（写侧保守：不在无快照会话上凭空造快照）。
type Snapshot struct {
	Found    bool
	Turn     string
	Messages []Message
}

// SnapshotStore 是会话快照读写端口（写 = 摘要 + snapshot_turn **同一事务**）。
type SnapshotStore interface {
	Current(ctx context.Context) (Snapshot, error)
	Commit(ctx context.Context, snap Snapshot) error
}

// Summarizer 是摘要生成端口（宿主用进程内单轮 LLM 实现；不新开 HTTP 通道）。
type Summarizer interface {
	Summarize(ctx context.Context, msgs []Message) (string, error)
}

// ─── 任务树端口（SESSION-1 阶段二·任务树）────────────────────────────

// TaskStore 是任务树**数据面**端口（读 / 落库 / 删）。
//
// 单写者口径（21-任务层设计方案 §9.2）：权威行 `tasktree` 由宿主任务层经既有
// `data-tasktree-*` 消息面维护，本组件只持内存运行态视图、**不自写库**。组件经本端口
// 做两件需要数据面参与的事：
//
//	AuthorizeCancel ← 读：层判定该子树中「可取消」的节点 id（nil = 层不可知 → 按内存视图判定）
//	RecoverStale    ← 收敛：启动恢复把库中非终态标 interrupted，返回标注条数
//
// 落库 / 删除仍由宿主单写者（任务层订阅事件）完成，组件不重复写库。
type TaskStore interface {
	AuthorizeCancel(instanceID, rootID, topSession string) []string
	RecoverStale(instanceID string) int
}

// TaskEmitter 是任务树事件广播端口（宿主 publish `tasks.started` / `tasks.updated` /
// `tasks.done`，载荷 = 完整节点快照，61 §3.4 零变更）。
type TaskEmitter interface {
	Started(node TaskNode)
	Updated(node TaskNode)
	Done(node TaskNode)
}

// TaskExecSink 是任务执行侧取消端口（宿主 gateway 执行池；取消经层回调或兜底直调）。
type TaskExecSink interface {
	CancelExec(instanceID, gwTaskID string) (bool, error)
}

// InstanceWorkDir 是实例 work_dir 解析端口（建节点时补齐隔离键；未注册 → false）。
type InstanceWorkDir interface {
	WorkDir(instanceID string) (workDir string, ok bool)
}
