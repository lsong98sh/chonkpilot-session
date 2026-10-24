// 任务树（SESSION-1 阶段二·任务树）：任务树的**纯逻辑**组件。
//
// 定位（依赖反置）：本组件**不**感知 MQ / DB / 配置文件——所有副作用都经 `ports.go` 的端口
// 由宿主（`src/lib/llm/server`）实现：
//
//	TaskEmitter     ← 广播 tasks.started / tasks.updated / tasks.done
//	TaskStore       ← 任务层权威读（取消判定）/ 启动恢复收敛
//	TaskExecSink    ← 执行侧取消（gateway 执行池；可选）
//	InstanceWorkDir ← 建节点时补齐 work_dir（可选）
//
// 本组件只持**内存运行态视图**（`nodes` / `order` / `lastEmit` / `subParents`）；权威行
// `tasktree` 由宿主任务层单写者经既有 `data-tasktree-*` 消息面维护（21-任务层设计方案 §9.2）。
//
// 行为必须与搬出前 `src/lib/llm/server/tasks.go` **逐字节等价**：节点字段、状态流转、
// 广播时机（started / updated / done）、错误文案、实例归属过滤、上限与 FIFO 淘汰。
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// TaskKind 任务节点类型（21-llm-server：kind=工具/llm/ask_user）。
const (
	TaskKindTool    = "tool"     // 普通工具（同步/异步，经 gateway tools/call 执行）
	TaskKindLLM     = "llm"      // 历史 llm 型节点 kind（当前无工具产出；保留取值兼容既有数据）
	TaskKindAskUser = "ask_user" // ask_user 工具 → server ask 通道（广播 ask-user → 等 reply → 回填）
)

// DSL 展示节点 kind（DSL-3，[42 §2 (251)/(252)]；61 §3.4 增补取值）：
//   - `dsl_job`      = llm_run 作业根节点（原 kind=llm 的根，DSL 展示入口）；
//   - `dsl_loop`     = LOOP 折叠容器节点（静态语句，**不随迭代增长**）；
//   - `dsl_parallel` = PARALLEL 折叠容器节点（同上）；
//   - `dsl_step`     = 单次执行记录 kind（**保留取值**：本实现选择「记录入 `dsl_job.steps[]`」
//     方案，不建树节点，故当前不产出；见 jobdsl.go buildStaticTree 注释）。
const (
	TaskKindDslJob      = "dsl_job"
	TaskKindDslLoop     = "dsl_loop"
	TaskKindDslParallel = "dsl_parallel"
	TaskKindDslStep     = "dsl_step"
)

// TaskState 任务节点状态（终态 done/error/cancelled/interrupted，对齐 21-llm-server）。
const (
	TaskStatePending     = "pending"     // 转后台（gateway 返回 pending{task_id}）
	TaskStateRunning     = "running"     // 执行中（tasks.started 初始态）
	TaskStateDone        = "done"        // 完成
	TaskStateError       = "error"       // 失败
	TaskStateCancelled   = "cancelled"   // 用户主动取消（task-stop 级联）
	TaskStateInterrupted = "interrupted" // 进程重启遗留（启动恢复标注；可重试，同 cancelled 语义）
)

// DslStepRecord 是 DSL 作业的一次 LLM 步骤执行记录（DSL-3：`dsl_job.steps[]` 元素，扁平、
// 不随迭代进树；`No` 跨迭代累计、1 起）。前端据 `session_id` 做「查看/新窗口」。
type DslStepRecord struct {
	No          int    `json:"no"`                     // 跨迭代累计序号（1 起）
	Status      string `json:"status"`                 // running / done / error / cancelled
	Purpose     string `json:"purpose"`                // 该步运行目的（展示名）
	ElapsedMs   int64  `json:"elapsed_ms"`             // 耗时（毫秒）
	CreatedAt   string `json:"created_at"`             // 开始时刻（ISO/RFC3339）
	SessionID   string `json:"session_id"`             // 该次执行的子会话 id（jobSession-N）
	StatementID string `json:"statement_id,omitempty"` // 所属静态语句节点 id（当前实现未产出，保留）
}

// TaskNode 是任务树节点（完整 task 结构，对齐 21-llm-server tasks.* 载荷；
// tool_pair 气泡/任务树全链路直取）。
type TaskNode struct {
	TaskID        string  `json:"task_id"`
	Tool          string  `json:"tool"`                  // 原始工具名（core_file_read / llm_run / ask_user…）
	ToolCallID    string  `json:"tool_call_id"`          // LLM tool-call id（前端 tool_pair 定位）
	ParentID      string  `json:"parent_id,omitempty"`   // 父任务节点 id；空 = 顶层
	TopSession    string  `json:"top_session,omitempty"` // 聚合根（主会话），树隔离
	Kind          string  `json:"kind"`                  // tool / llm / ask_user
	SessionID     string  `json:"session_id"`
	TurnID        string  `json:"turn_id"`
	InstanceID    string  `json:"instance_id"`
	WorkDir       string  `json:"work_dir,omitempty"`   // 隔离键（21 §1.2 D6：任务层管理单位 = work_dir；**载荷增补字段**，任务层落库用）
	Name          string  `json:"name"`                 // 展示名（LLM 填的 tool_call_display_name 优先，缺省工具名；llm 用 title）
	Purpose       string  `json:"purpose,omitempty"`    // 同展示名（LLM 必填的调用理由；转后台工具展示用）
	Simplified    string  `json:"simplified,omitempty"` // 简短摘要（气泡直取）
	State         string  `json:"state"`
	StartedAt     string  `json:"started_at"`
	FinishedAt    string  `json:"finished_at,omitempty"`
	Elapsed       int64   `json:"elapsed"`            // 秒（完整 task 结构固定字段，0 也下发）
	Progress      float64 `json:"progress,omitempty"` // 0~1（批量节点完成进度）
	Output        string  `json:"output,omitempty"`
	ResultSummary string  `json:"result_summary,omitempty"` // 终态摘要（done）
	Error         string  `json:"error,omitempty"`          // 终态错误（error/cancelled 的 summary 归此）
	Success       bool    `json:"success"`

	// Args 展示用工具参数（随 tasks.* 广播，供前端任务详情渲染）—— **仅 ask_user 节点填充**
	// （追问内容/选项等；I-50）。其余节点的调用参数保留在内部 args（tool-retry 用）不广播，
	// 避免每次任务事件的载荷膨胀。
	Args map[string]any `json:"args,omitempty"`

	// ── DSL 展示字段（DSL-3，[42 §2 (251)/(252)]；61 §3.4 增补，缺省为空 → 与既有载荷逐字节等价）──
	//
	// LoopCurrent / LoopTotal 仅容器节点（kind=dsl_loop/dsl_parallel）：当前轮次（1 起）/ 总轮数
	// （未知可缺省 0）。Steps 仅作业根节点（kind=dsl_job）：扁平步骤执行记录（不随迭代进树）。
	// Shadow = true 表示「执行记录隐藏节点」（本实现选择记录入 steps[]，不产出 shadow 节点）。
	// ReturnKind/Inline/File/Size = `$RETURN` 两态（inline 全文 / file 文件名 + 字节数）。
	LoopCurrent  int             `json:"loop_current,omitempty"`
	LoopTotal    int             `json:"loop_total,omitempty"`
	Steps        []DslStepRecord `json:"steps,omitempty"`
	Shadow       bool            `json:"shadow,omitempty"`
	ReturnKind   string          `json:"return_kind,omitempty"`
	ReturnInline string          `json:"return_inline,omitempty"`
	ReturnFile   string          `json:"return_file,omitempty"`
	ReturnSize   int             `json:"return_size,omitempty"`

	// 内部字段（不广播）
	started  time.Time      // 开始时刻（elapsed 计算）
	gwTaskID string         // gateway 异步任务 id（pending{task_id}；执行侧取消/转后台与完成回报定位）
	args     map[string]any // 工具调用参数（tool-retry 重跑用，不广播）
}

// Clone 返回节点快照（广播用；elapsed 实时刷新）。
func (n *TaskNode) Clone() *TaskNode {
	c := *n
	if !c.started.IsZero() {
		c.Elapsed = int64(time.Since(c.started).Seconds())
	}
	return &c
}

// ToolArgs 返回工具调用参数（tool-retry 重跑用；不随事件广播）。
func (n *TaskNode) ToolArgs() map[string]any { return n.args }

// SetToolArgs 设置工具调用参数（tool-retry 重跑用；不随事件广播）。
func (n *TaskNode) SetToolArgs(args map[string]any) { n.args = args }

// TerminalState 判断是否终态（done/error/cancelled；防重复广播）。
func TerminalState(state string) bool {
	return state == TaskStateDone || state == TaskStateError || state == TaskStateCancelled
}

// IsAskTool 判断工具是否为 ask 型（ask_user / user_ask / 含 ask_user）。
func IsAskTool(name string) bool {
	return name == "ask_user" || name == "user_ask" || strings.Contains(name, "ask_user")
}

// AskQuestion 从 ask_user 参数取问题文本（缺省「请回答」）。
func AskQuestion(args map[string]any) string {
	if q, ok := args["question"].(string); ok {
		return q
	}
	if q, ok := args["prompt"].(string); ok {
		return q
	}
	return "请回答"
}

// TaskKindOf 工具 → 任务节点 kind（ask_user / 普通工具）。
// `dsl_run` 的节点由 gateway 展示上报器创建（kind=dsl_job），不经本函数。
func TaskKindOf(tool string) string {
	if IsAskTool(tool) {
		return TaskKindAskUser
	}
	return TaskKindTool
}

// TaskDisplay 任务节点展示字段（标题取值优先级，对齐 34-任务 / I-38）：
//
//	① 调用参数注入的展示名 tool_call_display_name（gateway 注入、LLM 必填）——最优先（name/purpose）；
//	② 回退工具契约名（调用方须传 **契约名**：网关暴露名 self_<契约名> 已剥前缀）。
//
// simplified=简短摘要（气泡直取；ask 型取问题文本）。返回 (name, purpose, simplified)。
func TaskDisplay(tool string, args map[string]any) (string, string, string) {
	purpose, _ := args["tool_call_display_name"].(string) // ① 注入展示名
	name := purpose
	if name == "" {
		name = tool // ② 回退工具契约名
	}
	var simplified string
	switch {
	case IsAskTool(tool):
		simplified = AskQuestion(args)
	default:
		simplified = purpose
	}
	if simplified == "" {
		simplified = name
	}
	return name, purpose, simplified
}

// maxNodes 保留任务节点上限（防无限增长；对齐主仓库 TaskManager maxTasks=200）。
const maxNodes = 200

// maxActiveNodes 活跃任务上限（running/pending 非终态，超出时 Start 返回错误）。
const maxActiveNodes = 50

// matchInstance 判定任务节点的实例归属是否命中（2026-09-19 缺口 3）：
// instanceID 为空 = 不校验（旧调用方 / 旧语义，行为与引入前一致）；非空 → 节点须同 instance。
// 任务节点一律携带 InstanceID（turn.go / jobdsl.go / domainmcp.go 建节点时从 turn 继承）。
func matchInstance(instanceID, nodeInstance string) bool {
	return instanceID == "" || nodeInstance == instanceID
}

// GWTaskBinding 是节点与 gateway 异步任务的绑定结果（宿主据此登记任务层执行句柄）。
type GWTaskBinding struct {
	TaskID     string
	GWTaskID   string
	InstanceID string
	Tool       string
}

// TaskTreeOptions 是任务树的端口注入（全部可选；nil = 对应副作用不生效）。
type TaskTreeOptions struct {
	// Emitter 事件广播端口（nil = 不广播）。
	Emitter TaskEmitter
	// Store 任务树数据面端口（nil = 层不可知 → 取消判定按内存视图）。
	Store TaskStore
	// Exec 返回当前执行池控制面（nil 返回 / 未注入 = 执行侧不可取消）。
	// 取值为函数：宿主执行池可能在构造之后才装配（`Server.New` 顺序）。
	Exec func() TaskExecSink
	// WorkDir 实例 work_dir 解析（nil = 不补 work_dir）。
	WorkDir InstanceWorkDir
	// Logf 告警输出（nil = 丢弃）。
	Logf func(format string, args ...any)
}

// TaskTree 是任务树内存运行态视图（并发安全）。
type TaskTree struct {
	emitter TaskEmitter
	store   TaskStore
	exec    func() TaskExecSink
	workDir InstanceWorkDir
	logf    func(format string, args ...any)

	mu       sync.Mutex
	nodes    map[string]*TaskNode // task_id → 节点（树由 ParentID 表达）
	order    []string             // 插入序（终态节点 FIFO 淘汰，maxNodes 保护）
	lastEmit map[string]time.Time // task_id → 上次 updated 时间（节流 ≤250ms）

	// subParents 是 DSL-3 步骤子会话 → 静态父节点 id 映射（session_id → 容器/作业根 task_id）：
	// 执行记录不建树节点（DSL-3），子会话内产生的工具节点据此挂到该步所属**静态语句节点**
	// （容器 dsl_loop/dsl_parallel，或作业根 dsl_job）。仅内存（与作业同生命周期）。
	subParents map[string]string
}

// NewTaskTree 构造任务树（不启动任何 goroutine；后台清理由宿主定时调用 CleanupTerminal）。
func NewTaskTree(opts TaskTreeOptions) *TaskTree {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &TaskTree{
		emitter:    opts.Emitter,
		store:      opts.Store,
		exec:       opts.Exec,
		workDir:    opts.WorkDir,
		logf:       logf,
		nodes:      make(map[string]*TaskNode),
		lastEmit:   make(map[string]time.Time),
		subParents: make(map[string]string),
	}
}

// Clear 清空内存运行态视图（重启 / 用例隔离用；不广播、不写库）。
func (tt *TaskTree) Clear() {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.nodes = make(map[string]*TaskNode)
	tt.order = nil
	tt.lastEmit = make(map[string]time.Time)
	tt.subParents = make(map[string]string)
}

// Start 建任务节点并广播 tasks.started（state=running；启动后不可变字段由调用方先填）。
// 当**同 instance** 活跃任务数 >= maxActiveNodes 时返回错误（调用方应视为工具执行失败）——
// 2026-09-19 缺口 3：活跃数按 instance 归属统计，一个 instance 打满不再阻塞其他 instance。
// 落库由宿主任务层订阅 tasks.started 完成（本组件不自写库）。
func (tt *TaskTree) Start(n *TaskNode) (*TaskNode, error) {
	n.TaskID = "tk-" + newTaskID()
	n.State = TaskStateRunning
	n.StartedAt = time.Now().UTC().Format(time.RFC3339)
	n.started = time.Now()
	if n.Simplified == "" {
		n.Simplified = n.Name
	}
	if n.WorkDir == "" && tt.workDir != nil {
		if wd, ok := tt.workDir.WorkDir(n.InstanceID); ok {
			n.WorkDir = wd // work_dir 随 instance 绑定
		}
	}
	tt.mu.Lock()
	// 活跃任务数检查（running/pending 非终态；限本 instance 归属，缺口 3）
	active := 0
	for _, on := range tt.nodes {
		if !TerminalState(on.State) && matchInstance(n.InstanceID, on.InstanceID) {
			active++
		}
	}
	if active >= maxActiveNodes {
		tt.mu.Unlock()
		return nil, fmt.Errorf("活跃任务数已达上限（%d），请等待当前任务完成", maxActiveNodes)
	}
	tt.nodes[n.TaskID] = n
	tt.order = append(tt.order, n.TaskID)
	// 终态节点超出上限 → FIFO 淘汰（运行中不淘汰，放回队尾）
	for len(tt.order) > maxNodes {
		old := tt.order[0]
		tt.order = tt.order[1:]
		on, ok := tt.nodes[old]
		if !ok {
			continue
		}
		if !TerminalState(on.State) {
			tt.order = append(tt.order, old)
			break
		}
		delete(tt.nodes, old)
		delete(tt.lastEmit, old)
	}
	tt.mu.Unlock()
	tt.emitStarted(n.Clone())
	return n, nil
}

// Get 返回节点快照（不存在 → nil）。
func (tt *TaskTree) Get(taskID string) *TaskNode {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	n, ok := tt.nodes[taskID]
	if !ok {
		return nil
	}
	return n.Clone()
}

// SetState 状态变更（如 async 转后台 pending）：始终广播 tasks.updated。
func (tt *TaskTree) SetState(taskID, state string) {
	tt.mu.Lock()
	n, ok := tt.nodes[taskID]
	if !ok {
		tt.mu.Unlock()
		return
	}
	n.State = state
	tt.lastEmit[taskID] = time.Now()
	snap := n.Clone()
	tt.mu.Unlock()
	tt.emitUpdated(snap)
}

// Update 就地更新节点字段并广播 tasks.updated（派生后才确定的归属字段，
// 如 llm 节点的 session_id/turn_id = 子会话；始终广播，非进度节流路径）。
func (tt *TaskTree) Update(taskID string, fn func(*TaskNode)) {
	tt.mu.Lock()
	n, ok := tt.nodes[taskID]
	if !ok {
		tt.mu.Unlock()
		return
	}
	fn(n)
	tt.lastEmit[taskID] = time.Now()
	snap := n.Clone()
	tt.mu.Unlock()
	tt.emitUpdated(snap)
}

// SetGWTask 登记 gateway 异步任务 id（gateway 返回 pending 后；执行侧取消/转后台与完成回报定位）。
// 第二个返回值 = 节点存在（宿主据此登记任务层执行句柄）。
func (tt *TaskTree) SetGWTask(taskID, gwTaskID string) (GWTaskBinding, bool) {
	tt.mu.Lock()
	n, ok := tt.nodes[taskID]
	var binding GWTaskBinding
	if ok {
		n.gwTaskID = gwTaskID
		binding = GWTaskBinding{TaskID: taskID, GWTaskID: gwTaskID, InstanceID: n.InstanceID, Tool: n.Tool}
	}
	tt.mu.Unlock()
	if !ok {
		return GWTaskBinding{}, false
	}
	return binding, true
}

// NodeIn 按 task_id 取节点并校验实例归属（缺口 3）：instanceID 空 = 不校验（旧语义）；
// 节点不存在或归属不符 → nil。用于「已知调用方实例」的直取路径（不跨 instance 命中）。
func (tt *TaskTree) NodeIn(instanceID, taskID string) *TaskNode {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	n, ok := tt.nodes[taskID]
	if !ok || !matchInstance(instanceID, n.InstanceID) {
		return nil
	}
	return n.Clone()
}

// FindByGWTask 按 gateway 任务 id 找节点快照（异步完成回报定位；未登记 → nil）。
// instanceID 非空 → 只命中**该 instance 归属**的节点（缺口 3：回报按实例隔离）。
func (tt *TaskTree) FindByGWTask(instanceID, gwTaskID string) *TaskNode {
	if gwTaskID == "" {
		return nil
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	for _, n := range tt.nodes {
		if n.gwTaskID != gwTaskID || !matchInstance(instanceID, n.InstanceID) {
			continue
		}
		return n.Clone()
	}
	return nil
}

// FindByToolCall 按 LLM tool-call id 找任务节点（G-17 转后台门控反查工具名；未命中 → nil）。
// instanceID 非空 → 只命中该 instance 归属的节点（缺口 3）。
func (tt *TaskTree) FindByToolCall(instanceID, toolCallID string) *TaskNode {
	if toolCallID == "" {
		return nil
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	for i := len(tt.order) - 1; i >= 0; i-- {
		n, ok := tt.nodes[tt.order[i]]
		if ok && n.ToolCallID == toolCallID && matchInstance(instanceID, n.InstanceID) {
			return n.Clone()
		}
	}
	return nil
}

// Find 按 task_id 找节点（tool-retry 直取定位；未登记 → nil）。
func (tt *TaskTree) Find(taskID string) *TaskNode {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	n, ok := tt.nodes[taskID]
	if !ok {
		return nil
	}
	return n.Clone()
}

// LatestLLMSessionNode 按（top_session, session）定位子会话对应的父任务节点：
//   - **DSL-3 优先**：`subParents[session]`（由 jobdsl.llmAction 登记）→ 该步所属**静态语句节点**
//     （容器 dsl_loop/dsl_parallel 或作业根 dsl_job）。DSL-3 起执行记录不建树节点，子会话内产生的
//     工具/任务节点据此挂到静态语句节点之下（任务树拓扑：语句节点 → 该步工具）。
//   - 回落（既有语义）：kind=llm 且 SessionID=子会话 的节点（jobdsl 历史 / 单测手工构造）。
//
// 取最近创建（order 插入序逆序首个）；未命中 → nil（退化为顶层）。
// instanceID 非空 → 只命中该 instance 归属的节点（缺口 3：不跨 instance 挂错父节点）。
func (tt *TaskTree) LatestLLMSessionNode(instanceID, topSession, session string) *TaskNode {
	if session == "" {
		return nil
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if pid := tt.subParents[session]; pid != "" {
		if n, ok := tt.nodes[pid]; ok && matchInstance(instanceID, n.InstanceID) &&
			(topSession == "" || n.TopSession == topSession) {
			return n.Clone()
		}
	}
	for i := len(tt.order) - 1; i >= 0; i-- {
		n, ok := tt.nodes[tt.order[i]]
		if !ok || (n.Kind != TaskKindLLM && n.Kind != TaskKindDslStep) || n.SessionID != session {
			continue
		}
		if !matchInstance(instanceID, n.InstanceID) {
			continue
		}
		if topSession != "" && n.TopSession != topSession {
			continue
		}
		return n.Clone()
	}
	return nil
}

// BindSubSession 登记 DSL-3 步骤子会话 → 静态父节点 id（jobdsl.llmAction 在建子会话前调用）：
// 子会话内产生的工具节点据此挂到该步所属静态语句节点（容器或作业根）。
func (tt *TaskTree) BindSubSession(session, parentID string) {
	if session == "" || parentID == "" {
		return
	}
	tt.mu.Lock()
	tt.subParents[session] = parentID
	tt.mu.Unlock()
}

// UnbindSubSessions 清理某作业的全部子会话映射（作业结束时调用；防 map 无界增长）。
func (tt *TaskTree) UnbindSubSessions(sessions []string) {
	if len(sessions) == 0 {
		return
	}
	tt.mu.Lock()
	for _, s := range sessions {
		delete(tt.subParents, s)
	}
	tt.mu.Unlock()
}

// FindRetryTarget 定位 tool-retry 目标节点（msg-ref §4.2：消息区「重试」最后一条 interrupted
// 工具 pair）：候选须属该 session（turn/toolCallID 给定则进一步精确匹配）且状态为 interrupted
// （可重试；done/error/cancelled = 已有结果/主动取消不重跑，running/pending = 仍在执行不重试）；
// 多候选取最近启动（order 逆序首个，同前端"最后一条 interrupted"语义）。
// 返回节点快照（含内部 args，重跑用）；未命中 → nil。
// instanceID 非空 → 只命中该 instance 归属的节点（缺口 3：不跨 instance 重跑他人任务）。
func (tt *TaskTree) FindRetryTarget(instanceID, session, turn, toolCallID string) *TaskNode {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	for i := len(tt.order) - 1; i >= 0; i-- {
		n, ok := tt.nodes[tt.order[i]]
		if !ok || n.SessionID != session || n.State != TaskStateInterrupted {
			continue
		}
		if !matchInstance(instanceID, n.InstanceID) {
			continue
		}
		if turn != "" && n.TurnID != turn {
			continue
		}
		if toolCallID != "" && n.ToolCallID != toolCallID {
			continue
		}
		return n.Clone()
	}
	return nil
}

// RestoreStale tool-retry 重启恢复：把从库还原的节点登记回内存任务表（沿用原 task_id，
// 保证重跑终态广播/落库落在原节点）。不广播——随后 RetryReset 会广播 running。
// 节点已在内存（幂等）→ 跳过。
func (tt *TaskTree) RestoreStale(n *TaskNode) {
	if n == nil || n.TaskID == "" {
		return
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := tt.nodes[n.TaskID]; ok {
		return
	}
	n.State = TaskStateInterrupted // 遗留可重试语义（RetryReset 置 running）
	n.started = time.Now()
	if n.StartedAt == "" {
		n.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	tt.nodes[n.TaskID] = n
	tt.order = append(tt.order, n.TaskID)
}

// RetryReset 把 interrupted 节点重置为 running（tool-retry 重跑：复用原任务节点，不新建，
// 不动 args/树结构，重跑终态仍走原节点广播）；非 interrupted → false（不可重试）。
// 广播 tasks.updated（前端任务树/气泡可见重新运行）；层侧状态单调校验允许 interrupted → running
// （21 §9.2：interrupted 为可恢复终态）。
func (tt *TaskTree) RetryReset(taskID string) bool {
	tt.mu.Lock()
	n, ok := tt.nodes[taskID]
	if !ok || n.State != TaskStateInterrupted {
		tt.mu.Unlock()
		return false
	}
	n.State = TaskStateRunning
	n.FinishedAt = ""
	n.Error = ""
	n.ResultSummary = ""
	n.Success = false
	n.started = time.Now()
	snap := n.Clone()
	tt.mu.Unlock()
	tt.emitUpdated(snap)
	return true
}

// Progress 进度/输出更新：节流广播 tasks.updated（≤250ms，避免刷爆前端通道）。
func (tt *TaskTree) Progress(taskID string, progress float64, output string) {
	tt.mu.Lock()
	n, ok := tt.nodes[taskID]
	if !ok {
		tt.mu.Unlock()
		return
	}
	n.Progress = progress
	if output != "" {
		n.Output = output
	}
	now := time.Now()
	if now.Sub(tt.lastEmit[taskID]) < 250*time.Millisecond {
		tt.mu.Unlock()
		return
	}
	tt.lastEmit[taskID] = now
	snap := n.Clone()
	tt.mu.Unlock()
	tt.emitUpdated(snap)
}

// Done 终态（done/error/cancelled）：写 finished_at/elapsed/success → 广播 tasks.done（完整结构）。
// 幂等：已终态不重复广播。落库由宿主任务层订阅 tasks.done 完成（本组件不自写库）。
func (tt *TaskTree) Done(taskID, state, summary, errMsg string) {
	tt.mu.Lock()
	n, ok := tt.nodes[taskID]
	if !ok {
		tt.mu.Unlock()
		return
	}
	if TerminalState(n.State) {
		tt.mu.Unlock()
		return
	}
	n.State = state
	n.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if !n.started.IsZero() {
		n.Elapsed = int64(time.Since(n.started).Seconds())
	}
	n.Success = state == TaskStateDone
	if errMsg != "" {
		n.Error = errMsg
	}
	if summary != "" {
		if state == TaskStateDone {
			n.ResultSummary = summary
		} else {
			n.Error = summary // error/cancelled：摘要归入 error 字段（§3.3 result_summary/error）
		}
	}
	snap := n.Clone()
	tt.mu.Unlock()
	tt.emitDone(snap)
}

// Subtree 返回该节点及全部后代（级联定位：沿 parent_id 遍历）。
// instanceID 非空 → 根与后代都须同 instance（缺口 3：不跨 instance 级联到他人节点）。
func (tt *TaskTree) Subtree(instanceID, rootID string) []*TaskNode {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	var out []*TaskNode
	if n, ok := tt.nodes[rootID]; ok && matchInstance(instanceID, n.InstanceID) {
		out = append(out, n.Clone())
	}
	var walk func(id string)
	walk = func(id string) {
		for _, n := range tt.nodes {
			if n.ParentID == id && matchInstance(instanceID, n.InstanceID) {
				out = append(out, n.Clone())
				walk(n.TaskID)
			}
		}
	}
	walk(rootID)
	return out
}

// CancelSubtree 用户取消（task-stop）：**状态判定以层为权威**（21 §9.2 交付物 ⑤）——
// 层判定该子树中「非不可逆终态」的节点集合（`TaskStore.AuthorizeCancel`），并**由层回调执行侧**
// 取消；本方法只把层认可的节点标 cancelled 并逐个广播 tasks.done（事件名 / 载荷不变，cascade 沿
// parent_id 下发）。返回取消节点数。
//
// 层对该根节点一无所知（跨进程未登记 / 无数据面）时**退化为内存视图判定**（既有行为等价），
// 并直接下发执行侧取消（不经层回调）。
// instanceID 非空 → 只取消该 instance 归属的子树节点（缺口 3：不跨 instance 取消他人任务）。
func (tt *TaskTree) CancelSubtree(instanceID, rootID string) int {
	nodes := tt.Subtree(instanceID, rootID)
	if len(nodes) == 0 {
		// 本层（内存任务视图）未知该节点：它可能由**任务层/外部上报器**持有——如 gateway
		// 的 `dsl_job` 作业根只经 task-* 事件上报，不进入内存视图。此时仍以任务层为权威判定，
		// 并由层回调执行侧取消；本层无节点快照 → 不重复广播终态（终态事件由节点 owner 发）。
		if tt.store != nil {
			return len(tt.store.AuthorizeCancel(instanceID, rootID, ""))
		}
		return 0
	}
	topSession := ""
	for _, n := range nodes {
		if n.TaskID == rootID {
			topSession = n.TopSession
			break
		}
	}
	var allowed map[string]bool
	if tt.store != nil {
		if ids := tt.store.AuthorizeCancel(instanceID, rootID, topSession); ids != nil {
			allowed = make(map[string]bool, len(ids))
			for _, id := range ids {
				allowed[id] = true
			}
		}
	}
	cancelled := 0
	for _, n := range nodes {
		if allowed != nil {
			if !allowed[n.TaskID] {
				continue // 层为准：层判定不可取消（不可逆终态）→ 不重复广播
			}
		} else if TerminalState(n.State) {
			continue // 层不可知 → 内存视图判定（既有行为）
		}
		if allowed == nil && n.gwTaskID != "" {
			// 层不可知兜底：由本层直接下发执行侧取消（既有路径）
			if sink := tt.execSink(); sink != nil {
				if ok, err := sink.CancelExec(n.InstanceID, n.gwTaskID); err != nil || !ok {
					tt.logf("执行侧取消失败（task=%s gw_task=%s）: ok=%v err=%v", n.TaskID, n.gwTaskID, ok, err)
				}
			}
		}
		tt.Done(n.TaskID, TaskStateCancelled, "", "")
		cancelled++
	}
	return cancelled
}

// CancelByTurn 取消某轮次在飞的**全部**任务节点（I-35：llm-cancel 级联，不再只终止轮次）。
// 按 (instance, TurnID) 归属定位非终态节点 → 逐个 CancelSubtree（含其子树；done 幂等，重叠子树
// 不重复广播）。instanceID 非空 → 只命中该 instance（缺口 3：同名 turn 跨 instance 不互相取消）。
// 返回取消节点数（执行侧取消经层回调下发，不再逐个回传 gateway task id）。
func (tt *TaskTree) CancelByTurn(instanceID, turnID string) int {
	if turnID == "" {
		return 0
	}
	tt.mu.Lock()
	var targets []string
	for id, n := range tt.nodes {
		if n.TurnID == turnID && !TerminalState(n.State) && matchInstance(instanceID, n.InstanceID) {
			targets = append(targets, id)
		}
	}
	tt.mu.Unlock()
	var total int
	for _, id := range targets {
		total += tt.CancelSubtree(instanceID, id)
	}
	return total
}

// MarkDeleted 记录节点被前端手动关闭：**内存视图移除**（不再广播/不再参与级联）。
// 库侧由宿主 persist `data-tasktree-delete` 逻辑删除（closed 标记）完成。
func (tt *TaskTree) MarkDeleted(taskID string) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	delete(tt.nodes, taskID)
	for i, id := range tt.order {
		if id == taskID {
			tt.order = append(tt.order[:i], tt.order[i+1:]...)
			break
		}
	}
	delete(tt.lastEmit, taskID)
}

// CleanupTerminal 清理已终态超过 5 分钟的节点（宿主定时调用）。
func (tt *TaskTree) CleanupTerminal() {
	cutoff := time.Now().Add(-5 * time.Minute)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	var keep []string
	for _, id := range tt.order {
		n, ok := tt.nodes[id]
		if !ok {
			continue
		}
		if TerminalState(n.State) {
			if n.FinishedAt != "" {
				ft, err := time.Parse(time.RFC3339, n.FinishedAt)
				if err == nil && ft.Before(cutoff) {
					delete(tt.nodes, id)
					delete(tt.lastEmit, id)
					continue
				}
			}
		}
		keep = append(keep, id)
	}
	tt.order = keep
}

// RecoverStale 启动 / 首个 llm-start 的遗留清理（R2，与 data-session-cleanup-stale 同触发点）：
// 交任务层把库中**非终态**（pending/running/awaiting/detached）节点一律标 `interrupted`
// （可重试；**不自动重跑**），closed 等既有标记原样保留。返回标注条数。
func (tt *TaskTree) RecoverStale(instanceID string) int {
	if instanceID == "" || tt.store == nil {
		return 0
	}
	return tt.store.RecoverStale(instanceID)
}

// execSink 取当前执行池控制面（宿主可能在构造后才装配；nil = 不可取消）。
func (tt *TaskTree) execSink() TaskExecSink {
	if tt.exec == nil {
		return nil
	}
	return tt.exec()
}

// ─── 广播出口（端口 nil 安全）─────────────────────────────────────

func (tt *TaskTree) emitStarted(n *TaskNode) {
	if tt.emitter != nil {
		tt.emitter.Started(*n)
	}
}

func (tt *TaskTree) emitUpdated(n *TaskNode) {
	if tt.emitter != nil {
		tt.emitter.Updated(*n)
	}
}

func (tt *TaskTree) emitDone(n *TaskNode) {
	if tt.emitter != nil {
		tt.emitter.Done(*n)
	}
}

// newTaskID 生成任务节点 id（格式 `tk-<8 字节 hex>`，与宿主既有 id 口径一致）。
func newTaskID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
