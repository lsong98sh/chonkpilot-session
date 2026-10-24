// 任务树组件单测（SESSION-1 阶段二·任务树）：
// 建/追加 / 状态推进 / 取消级联 / 终态收敛 / 进度统计 / 展示映射 / task_* 工具入参校验。
package session

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── 假端口 ─────────────────────────────────────────────

type fakeEmitter struct {
	mu      sync.Mutex
	started []TaskNode
	updated []TaskNode
	done    []TaskNode
}

func (e *fakeEmitter) Started(n TaskNode) {
	e.mu.Lock()
	e.started = append(e.started, n)
	e.mu.Unlock()
}
func (e *fakeEmitter) Updated(n TaskNode) {
	e.mu.Lock()
	e.updated = append(e.updated, n)
	e.mu.Unlock()
}
func (e *fakeEmitter) Done(n TaskNode) { e.mu.Lock(); e.done = append(e.done, n); e.mu.Unlock() }

func (e *fakeEmitter) counts() (int, int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.started), len(e.updated), len(e.done)
}

type fakeTaskStore struct {
	authKnown  bool     // false → AuthorizeCancel 返回 nil（层不可知）
	authIDs    []string // 层判定的可取消集合
	recoverN   int
	cancelArgs []string // 记录 AuthorizeCancel 入参（root）
}

func (s *fakeTaskStore) AuthorizeCancel(instanceID, rootID, topSession string) []string {
	s.cancelArgs = append(s.cancelArgs, rootID)
	if !s.authKnown {
		return nil
	}
	return s.authIDs
}

func (s *fakeTaskStore) RecoverStale(instanceID string) int { return s.recoverN }

type fakeExec struct {
	mu    sync.Mutex
	calls []string
	ok    bool
	err   error
}

func (e *fakeExec) CancelExec(instanceID, gwTaskID string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, gwTaskID)
	return e.ok, e.err
}

type fakeWorkDir struct{ dir string }

func (w fakeWorkDir) WorkDir(instanceID string) (string, bool) {
	if w.dir == "" {
		return "", false
	}
	return w.dir, true
}

// newTestTree 构造带记录端口的任务树。
func newTestTree(t *testing.T) (*TaskTree, *fakeEmitter, *fakeTaskStore, *fakeExec) {
	t.Helper()
	em := &fakeEmitter{}
	st := &fakeTaskStore{}
	ex := &fakeExec{ok: true}
	tt := NewTaskTree(TaskTreeOptions{
		Emitter: em,
		Store:   st,
		Exec:    func() TaskExecSink { return ex },
		WorkDir: fakeWorkDir{dir: "/work"},
	})
	return tt, em, st, ex
}

// ─── 建节点 / 追加 / 最近优先 ─────────────────────────────

func TestTaskTreeStartAndAppend(t *testing.T) {
	tt, em, _, _ := newTestTree(t)

	root, err := tt.Start(&TaskNode{
		Tool: "core_file_read", Kind: TaskKindTool, TopSession: "s1", SessionID: "s1",
		TurnID: "t1", InstanceID: "ins-1", Name: "根节点",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(root.TaskID, "tk-") || root.TaskID == "tk-" {
		t.Fatalf("task_id=%q want tk-<hex>", root.TaskID)
	}
	if root.State != TaskStateRunning {
		t.Fatalf("state=%q want running", root.State)
	}
	if root.StartedAt == "" {
		t.Fatal("started_at 必填")
	}
	if root.Simplified != "根节点" {
		t.Fatalf("simplified=%q want 根节点（缺省取 name）", root.Simplified)
	}
	if root.WorkDir != "/work" {
		t.Fatalf("work_dir=%q want /work（instance 绑定解析）", root.WorkDir)
	}
	if n, _, _ := em.counts(); n != 1 {
		t.Fatalf("started 事件 = %d, want 1", n)
	}

	// 追加子节点（ParentID 表达树）
	child, err := tt.Start(&TaskNode{
		Tool: "core_file_read", Kind: TaskKindTool, TopSession: "s1", SessionID: "s1",
		TurnID: "t1", InstanceID: "ins-1", ParentID: root.TaskID, Name: "子节点",
	})
	if err != nil {
		t.Fatalf("Start child: %v", err)
	}
	if nodes := tt.Subtree("ins-1", root.TaskID); len(nodes) != 2 {
		t.Fatalf("subtree = %d nodes, want 2（根 + 子）", len(nodes))
	}
	if nodes := tt.Subtree("ins-1", child.TaskID); len(nodes) != 1 {
		t.Fatalf("child subtree = %d nodes, want 1", len(nodes))
	}

	// 同 tool_call_id 多节点 → FindByToolCall 取最近创建（order 逆序）
	old, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, ToolCallID: "tc-x", InstanceID: "ins-1", Name: "old"})
	newest, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, ToolCallID: "tc-x", InstanceID: "ins-1", Name: "new"})
	if got := tt.FindByToolCall("ins-1", "tc-x"); got == nil || got.TaskID != newest.TaskID {
		t.Fatalf("FindByToolCall 命中 %+v, want 最近创建 %s（旧 %s）", got, newest.TaskID, old.TaskID)
	}

	// 实例隔离：其他 instance 不可见
	if got := tt.NodeIn("ins-2", root.TaskID); got != nil {
		t.Fatalf("跨 instance 直取应 nil，实际 %+v", got)
	}
	if got := tt.FindByToolCall("ins-2", "tc-x"); got != nil {
		t.Fatalf("跨 instance 按 tool_call 命中应 nil，实际 %+v", got)
	}
}

// TestTaskTreeLatestLLMSessionNode 子会话父节点定位（DSL 绑定优先 → kind=llm 回落）。
func TestTaskTreeLatestLLMSessionNode(t *testing.T) {
	tt, _, _, _ := newTestTree(t)
	llmNode, _ := tt.Start(&TaskNode{
		Tool: "llm_run", Kind: TaskKindLLM, SessionID: "sub-1", TopSession: "top",
		InstanceID: "ins-1", Name: "子步骤",
	})
	got := tt.LatestLLMSessionNode("ins-1", "top", "sub-1")
	if got == nil || got.TaskID != llmNode.TaskID {
		t.Fatalf("LatestLLMSessionNode=%+v want %s", got, llmNode.TaskID)
	}
	if got := tt.LatestLLMSessionNode("ins-2", "top", "sub-1"); got != nil {
		t.Fatalf("跨 instance 应 nil，实际 %+v", got)
	}
	// DSL 绑定优先（session → 静态父节点）
	container, _ := tt.Start(&TaskNode{Tool: "dsl_loop", Kind: TaskKindDslLoop, SessionID: "", TopSession: "top", InstanceID: "ins-1", Name: "循环"})
	tt.BindSubSession("sub-2", container.TaskID)
	if got := tt.LatestLLMSessionNode("ins-1", "top", "sub-2"); got == nil || got.TaskID != container.TaskID {
		t.Fatalf("bindSubSession 未生效: %+v", got)
	}
	tt.UnbindSubSessions([]string{"sub-2"})
	if got := tt.LatestLLMSessionNode("ins-1", "top", "sub-2"); got != nil {
		t.Fatalf("unbind 后应回落 nil，实际 %+v", got)
	}
}

// TestTaskTreeActiveLimit 活跃任务上限（同 instance >= 50 → 建节点失败）。
func TestTaskTreeActiveLimit(t *testing.T) {
	tt, _, _, _ := newTestTree(t)
	for i := 0; i < 50; i++ {
		if _, err := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "n"}); err != nil {
			t.Fatalf("start #%d: %v", i, err)
		}
	}
	_, err := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "overflow"})
	if err == nil || err.Error() != fmt.Sprintf("活跃任务数已达上限（%d），请等待当前任务完成", 50) {
		t.Fatalf("err=%v want 上限文案", err)
	}
	// 其他 instance 不受影响
	if _, err := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-2", Name: "other"}); err != nil {
		t.Fatalf("其他 instance 不应受限: %v", err)
	}
}

// TestTaskTreeTerminalFIFOEviction 终态节点超出上限 → FIFO 淘汰（运行中不淘汰）。
func TestTaskTreeTerminalFIFOEviction(t *testing.T) {
	tt, _, _, _ := newTestTree(t)
	var firstID string
	for i := 0; i < maxNodes; i++ {
		n, err := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "n"})
		if err != nil {
			t.Fatalf("start #%d: %v", i, err)
		}
		if i == 0 {
			firstID = n.TaskID
		}
		tt.Done(n.TaskID, TaskStateDone, "ok", "")
	}
	if tt.Get(firstID) == nil {
		t.Fatal("上限内不应淘汰")
	}
	if _, err := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "overflow"}); err != nil {
		t.Fatalf("start overflow: %v", err)
	}
	if tt.Get(firstID) != nil {
		t.Fatal("超出上限应 FIFO 淘汰最早终态节点")
	}
}

// ─── 状态推进 / 终态幂等 ─────────────────────────────

func TestTaskTreeStateTransitions(t *testing.T) {
	tt, em, _, _ := newTestTree(t)
	n, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "n"})

	tt.SetState(n.TaskID, TaskStatePending)
	if got := tt.Get(n.TaskID); got == nil || got.State != TaskStatePending {
		t.Fatalf("setState 未生效: %+v", got)
	}
	if _, upd, _ := em.counts(); upd != 1 {
		t.Fatalf("updated 事件 = %d, want 1（setState 始终广播）", upd)
	}

	tt.Done(n.TaskID, TaskStateDone, "结果", "")
	got := tt.Get(n.TaskID)
	if got == nil || got.State != TaskStateDone || !got.Success || got.ResultSummary != "结果" || got.FinishedAt == "" {
		t.Fatalf("done 终态字段不符: %+v", got)
	}
	_, _, done := em.counts()
	if done != 1 {
		t.Fatalf("done 事件 = %d, want 1", done)
	}
	// 幂等：已终态不重复广播、不改写
	tt.Done(n.TaskID, TaskStateError, "", "late")
	if _, _, done := em.counts(); done != 1 {
		t.Fatalf("重复 done 不应广播，实际 %d", done)
	}
	if got := tt.Get(n.TaskID); got.State != TaskStateDone || got.Error != "" {
		t.Fatalf("幂等后节点被改写: %+v", got)
	}

	// error：errMsg 与 summary 都归 error（summary 覆盖）
	e, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "e"})
	tt.Done(e.TaskID, TaskStateError, "失败摘要", "底层错误")
	if got := tt.Get(e.TaskID); got.Error != "失败摘要" || got.Success {
		t.Fatalf("error 终态不符: %+v", got)
	}

	// cancelled：摘要归 error
	c, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "c"})
	tt.Done(c.TaskID, TaskStateCancelled, "已取消", "")
	if got := tt.Get(c.TaskID); got.Error != "已取消" || got.Success {
		t.Fatalf("cancelled 终态不符: %+v", got)
	}
}

// ─── 取消级联 ─────────────────────────────────────

func TestTaskTreeCancelCascade(t *testing.T) {
	tt, em, _, ex := newTestTree(t)
	root, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", TopSession: "s1", TurnID: "t1", Name: "root"})
	tt.SetGWTask(root.TaskID, "gw-1")
	child, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", TopSession: "s1", TurnID: "t1", ParentID: root.TaskID, Name: "child"})
	tt.SetGWTask(child.TaskID, "gw-2")
	grand, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", TopSession: "s1", TurnID: "t1", ParentID: child.TaskID, Name: "gc"})

	// 层不可知（AuthorizeCancel → nil）→ 内存视图判定 + 兜底下发执行侧取消
	if got := tt.CancelSubtree("ins-1", root.TaskID); got != 3 {
		t.Fatalf("cancelled=%d want 3", got)
	}
	for _, id := range []string{root.TaskID, child.TaskID, grand.TaskID} {
		if n := tt.Get(id); n == nil || n.State != TaskStateCancelled || n.Success {
			t.Fatalf("节点 %s 未级联取消: %+v", id, n)
		}
	}
	if len(ex.calls) != 2 || ex.calls[0] != "gw-1" || ex.calls[1] != "gw-2" {
		t.Fatalf("执行侧取消 = %v, want [gw-1 gw-2]", ex.calls)
	}
	if _, _, done := em.counts(); done != 3 {
		t.Fatalf("done 事件 = %d, want 3", done)
	}
	// 幂等：再取消一次（全终态）→ 0（层不可知 + 内存全终态）
	if got := tt.CancelSubtree("ins-1", root.TaskID); got != 0 {
		t.Fatalf("重复取消 = %d, want 0", got)
	}

	// 层为权威：只取消层认可的节点（exec 不再兜底下发）
	tt2, _, store2, ex2 := newTestTree(t)
	r2, _ := tt2.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", TopSession: "s1", Name: "r2"})
	tt2.SetGWTask(r2.TaskID, "gw-x")
	c2, _ := tt2.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", TopSession: "s1", ParentID: r2.TaskID, Name: "c2"})
	store2.authKnown = true
	store2.authIDs = []string{r2.TaskID}
	if got := tt2.CancelSubtree("ins-1", r2.TaskID); got != 1 {
		t.Fatalf("层限定取消 = %d, want 1", got)
	}
	if n := tt2.Get(r2.TaskID); n.State != TaskStateCancelled {
		t.Fatalf("层认可节点应 cancelled: %+v", n)
	}
	if n := tt2.Get(c2.TaskID); n.State != TaskStateRunning {
		t.Fatalf("层未认可节点不应取消: %+v", n)
	}
	if len(ex2.calls) != 0 {
		t.Fatalf("层权威路径不应兜底下发执行侧取消: %v", ex2.calls)
	}

	// 根节点不在内存但层已知（外部上报器建的节点）→ 只问层，不广播终态
	store3 := &fakeTaskStore{authKnown: true, authIDs: []string{"tk-external", "tk-ext-child"}}
	em3 := &fakeEmitter{}
	tt3 := NewTaskTree(TaskTreeOptions{Emitter: em3, Store: store3})
	if got := tt3.CancelSubtree("ins-1", "tk-external"); got != 2 {
		t.Fatalf("层未知根取消 = %d, want 2", got)
	}
	if _, _, done := em3.counts(); done != 0 {
		t.Fatalf("层未知根不应广播终态，实际 %d", done)
	}
}

// TestTaskTreeCancelByTurn 按轮次取消在飞节点（其他轮次/实例不受影响）。
func TestTaskTreeCancelByTurn(t *testing.T) {
	tt, _, _, _ := newTestTree(t)
	a, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", TurnID: "t1", Name: "a"})
	b, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", TurnID: "t2", Name: "b"})
	c, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-2", TurnID: "t1", Name: "c"})
	if got := tt.CancelByTurn("ins-1", "t1"); got != 1 {
		t.Fatalf("cancelByTurn=%d want 1", got)
	}
	if n := tt.Get(a.TaskID); n.State != TaskStateCancelled {
		t.Fatalf("同 instance 同轮次应取消: %+v", n)
	}
	if n := tt.Get(b.TaskID); n.State != TaskStateRunning {
		t.Fatalf("其他轮次不应取消: %+v", n)
	}
	if n := tt.Get(c.TaskID); n.State != TaskStateRunning {
		t.Fatalf("其他 instance 不应取消: %+v", n)
	}
}

// ─── 终态收敛 / 重试 / 清理 ─────────────────────────

func TestTaskTreeTerminalConvergence(t *testing.T) {
	tt, _, store, _ := newTestTree(t)

	// 启动恢复：委托端口（instance 空 → 0）
	store.recoverN = 3
	if got := tt.RecoverStale("ins-1"); got != 3 {
		t.Fatalf("RecoverStale=%d want 3（委托任务层）", got)
	}
	if got := tt.RecoverStale(""); got != 0 {
		t.Fatalf("RecoverStale(空 instance)=%d want 0", got)
	}

	// 终态清理：完成超过 5 分钟的节点移除；运行中保留
	old, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "old"})
	tt.Done(old.TaskID, TaskStateDone, "ok", "")
	tt.Update(old.TaskID, func(n *TaskNode) {
		n.FinishedAt = time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339)
	})
	fresh, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "fresh"})
	tt.Done(fresh.TaskID, TaskStateDone, "ok", "")
	running, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "running"})
	tt.CleanupTerminal()
	if tt.Get(old.TaskID) != nil {
		t.Fatal("超 5 分钟终态节点应被清理")
	}
	if tt.Get(fresh.TaskID) == nil || tt.Get(running.TaskID) == nil {
		t.Fatal("未超时终态 / 运行中节点不应被清理")
	}
}

// TestTaskTreeRetryLifecycle 重启遗留登记 → interrupted → 重跑复位。
func TestTaskTreeRetryLifecycle(t *testing.T) {
	tt, em, _, _ := newTestTree(t)
	n := &TaskNode{TaskID: "tk-stale", Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1",
		SessionID: "s1", TurnID: "t1", ToolCallID: "tc-1", Name: "stale"}
	n.SetToolArgs(map[string]any{"path": "a"})
	tt.RestoreStale(n)
	if got := tt.Get("tk-stale"); got == nil || got.State != TaskStateInterrupted {
		t.Fatalf("restoreStale 应登记为 interrupted: %+v", got)
	}
	if _, started, _ := em.counts(); started != 0 {
		t.Fatalf("restoreStale 不应广播 started，实际 %d", started)
	}
	// 幂等
	tt.RestoreStale(&TaskNode{TaskID: "tk-stale", Name: "dup"})
	if got := tt.Get("tk-stale"); got.Name != "stale" {
		t.Fatalf("重复 restoreStale 不应覆盖: %+v", got)
	}
	// 定位重试目标
	if got := tt.FindRetryTarget("ins-1", "s1", "t1", "tc-1"); got == nil || got.TaskID != "tk-stale" {
		t.Fatalf("findRetryTarget 未命中: %+v", got)
	} else if got.ToolArgs()["path"] != "a" {
		t.Fatalf("重试目标应携带内部 args: %+v", got.ToolArgs())
	}
	if got := tt.FindRetryTarget("ins-1", "s1", "t1", "tc-other"); got != nil {
		t.Fatalf("tool_call 不匹配应 nil: %+v", got)
	}
	// 重跑复位
	if !tt.RetryReset("tk-stale") {
		t.Fatal("interrupted 应可复位")
	}
	if got := tt.Get("tk-stale"); got.State != TaskStateRunning || got.FinishedAt != "" || got.Error != "" || got.Success {
		t.Fatalf("retryReset 复位不完整: %+v", got)
	}
	if _, upd, _ := em.counts(); upd != 1 {
		t.Fatalf("retryReset 应广播一次 updated，实际 %d", upd)
	}
	if tt.RetryReset("tk-stale") {
		t.Fatal("非 interrupted 不可复位")
	}
}

// TestTaskTreeMarkDeleted 手动关闭：内存移除、不再参与查找。
func TestTaskTreeMarkDeleted(t *testing.T) {
	tt, _, _, _ := newTestTree(t)
	n, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", ToolCallID: "tc-d", Name: "n"})
	tt.MarkDeleted(n.TaskID)
	if tt.Get(n.TaskID) != nil || tt.FindByToolCall("ins-1", "tc-d") != nil {
		t.Fatal("markDeleted 后节点应不可见")
	}
}

// ─── 进度统计（节流 ≤250ms）─────────────────────────

func TestTaskTreeProgress(t *testing.T) {
	tt, em, _, _ := newTestTree(t)
	n, _ := tt.Start(&TaskNode{Tool: "t", Kind: TaskKindTool, InstanceID: "ins-1", Name: "n"})

	tt.Progress(n.TaskID, 0.5, "half")
	if _, upd, _ := em.counts(); upd != 1 {
		t.Fatalf("首次进度应广播，实际 %d", upd)
	}
	if got := tt.Get(n.TaskID); got.Progress != 0.5 || got.Output != "half" {
		t.Fatalf("进度字段不符: %+v", got)
	}
	// 节流窗口内：不广播但字段照常更新
	tt.Progress(n.TaskID, 0.9, "near")
	if _, upd, _ := em.counts(); upd != 1 {
		t.Fatalf("250ms 内不应重复广播，实际 %d", upd)
	}
	if got := tt.Get(n.TaskID); got.Progress != 0.9 || got.Output != "near" {
		t.Fatalf("节流期间字段应更新: %+v", got)
	}
	time.Sleep(260 * time.Millisecond)
	tt.Progress(n.TaskID, 1, "done")
	if _, upd, _ := em.counts(); upd != 2 {
		t.Fatalf("超节流窗口应广播，实际 %d", upd)
	}
	if got := tt.Get(n.TaskID); got.Progress != 1 || got.Output != "done" {
		t.Fatalf("最终进度不符: %+v", got)
	}
}

// ─── 展示映射 / 终态文案 ─────────────────────────

func TestTaskDisplayAndKind(t *testing.T) {
	if got := TaskKindOf("ask_user"); got != TaskKindAskUser {
		t.Fatalf("kind=%q want ask_user", got)
	}
	if got := TaskKindOf("core_file_read"); got != TaskKindTool {
		t.Fatalf("kind=%q want tool", got)
	}
	name, purpose, simp := TaskDisplay("tool_result", map[string]any{"tool_call_display_name": "派活说明"})
	if name != "派活说明" || purpose != "派活说明" || simp != "派活说明" {
		t.Fatalf("注入优先失败: %q/%q/%q", name, purpose, simp)
	}
	name, purpose, simp = TaskDisplay("tool_result", map[string]any{"id": "x"})
	if name != "tool_result" || simp != "tool_result" || purpose != "" {
		t.Fatalf("回退契约名失败: %q/%q/%q", name, purpose, simp)
	}
	name, _, simp = TaskDisplay("ask_user", map[string]any{"question": "确认继续？"})
	if name != "ask_user" || simp != "确认继续？" {
		t.Fatalf("ask 展示名失败: %q/%q", name, simp)
	}
	// AskQuestion 回退
	if got := AskQuestion(map[string]any{"prompt": "p"}); got != "p" {
		t.Fatalf("AskQuestion(prompt)=%q", got)
	}
	if got := AskQuestion(map[string]any{}); got != "请回答" {
		t.Fatalf("AskQuestion 缺省=%q", got)
	}
}

func TestTaskNodeResultText(t *testing.T) {
	cases := []struct {
		node  *TaskNode
		text  string
		state string
		ok    bool
	}{
		{nil, "", "", false},
		{&TaskNode{State: TaskStateRunning}, "", "", false},
		{&TaskNode{State: TaskStateDone, ResultSummary: "ok"}, "ok", "done", true},
		{&TaskNode{State: TaskStateError, Error: "boom"}, "任务失败: boom", "error", true},
		{&TaskNode{State: TaskStateCancelled, Error: "stop"}, "任务已取消: stop", "cancelled", true},
	}
	for i, c := range cases {
		text, state, ok := TaskNodeResultText(c.node)
		if text != c.text || state != c.state || ok != c.ok {
			t.Fatalf("case %d: got (%q,%q,%v) want (%q,%q,%v)", i, text, state, ok, c.text, c.state, c.ok)
		}
	}
	if AsyncResultUnavailableText != "异步结果已不可用（任务已终态，但 message 表无该调用的结果行）" {
		t.Fatalf("不可用文案变了: %q", AsyncResultUnavailableText)
	}
}

// ─── task_* 工具入参校验 / 结果文案 ─────────────────

func TestParseToolStopArgs(t *testing.T) {
	// process_id 一律拒绝（先于 task_id 校验）
	if _, text, isErr := ParseToolStopArgs(map[string]any{"process_id": float64(42), "task_id": "tk-1"}); !isErr ||
		text != "错误: 不支持按 process_id(42) 终止进程（本服务不登记子进程 PID，无法校验归属）；请改用 task_id 级联取消" {
		t.Fatalf("process_id 拒绝文案不符: %q (isErr=%v)", text, isErr)
	}
	// task_id 必填
	if _, text, isErr := ParseToolStopArgs(map[string]any{}); !isErr || text != "错误: task_id 必填（process_id 已不支持）" {
		t.Fatalf("task_id 必填文案不符: %q (isErr=%v)", text, isErr)
	}
	// 合法
	req, text, isErr := ParseToolStopArgs(map[string]any{"task_id": "tk-1"})
	if isErr || text != "" || req.TaskID != "tk-1" {
		t.Fatalf("合法入参解析不符: %+v %q %v", req, text, isErr)
	}
}

func TestParseToolResultArgs(t *testing.T) {
	if _, text, isErr := ParseToolResultArgs(map[string]any{}); !isErr || text != "错误: id 必填" {
		t.Fatalf("id 必填文案不符: %q (isErr=%v)", text, isErr)
	}
	if req, _, isErr := ParseToolResultArgs(map[string]any{"id": "x"}); isErr || req.TimeoutSec != 30 {
		t.Fatalf("缺省 timeout 应为 30: %+v", req)
	}
	if req, _, _ := ParseToolResultArgs(map[string]any{"id": "x", "timeout": float64(5)}); req.TimeoutSec != 5 {
		t.Fatalf("自定义 timeout 不符: %+v", req)
	}
	if req, _, _ := ParseToolResultArgs(map[string]any{"id": "x", "timeout": float64(-1)}); req.TimeoutSec != 0 {
		t.Fatalf("负 timeout 应收敛为 0: %+v", req)
	}
	if req, _, _ := ParseToolResultArgs(map[string]any{"id": "x", "timeout": float64(0)}); req.TimeoutSec != 0 {
		t.Fatalf("0 timeout 应保持 0: %+v", req)
	}
}

func TestTaskToolTexts(t *testing.T) {
	if got := ToolStopAlreadyDoneText("tk-1", TaskStateDone); got != "任务 tk-1 已结束（done），无需停止" {
		t.Fatalf("already done 文案: %q", got)
	}
	if got := ToolStopExecUnavailableText("tk-1"); got != "错误: 执行池不可用（未注入 sink），无法打断任务 tk-1" {
		t.Fatalf("exec unavailable 文案: %q", got)
	}
	if got := ToolStopNotFoundText("tk-1"); got != "任务 tk-1 不存在或已结束" {
		t.Fatalf("not found 文案: %q", got)
	}
	if got := ToolStopOKText("tk-1", 3); got != "🛑 任务 tk-1 已级联停止（取消 3 个节点）" {
		t.Fatalf("ok 文案: %q", got)
	}
	if got := ToolResultCancelledText("x"); got != "已取消: 任务 x 查询中止（gateway 执行侧取消；被查询任务本身未受影响，可重新调用 tool_result）" {
		t.Fatalf("cancelled 文案: %q", got)
	}
	if got := ToolResultRunningText("x", 30); got != `任务尚未结束：x 仍在运行（已等待 30s）。可再次调用 tool_result(id="x", timeout=更长秒数) 继续等待` {
		t.Fatalf("running 文案: %q", got)
	}
	if got := ToolResultNotFoundText("x"); got != "错误: 未找到任务 x" {
		t.Fatalf("not found 文案: %q", got)
	}
}

// TestTaskTreeCloneElapsed Clone 快照刷新 elapsed（广播口径）。
func TestTaskTreeCloneElapsed(t *testing.T) {
	n := &TaskNode{TaskID: "tk-1", Name: "n"}
	n.started = time.Now().Add(-2 * time.Second)
	c := n.Clone()
	if c.Elapsed != 2 {
		t.Fatalf("clone elapsed=%d want 2", c.Elapsed)
	}
	if c == n {
		t.Fatal("clone 必须返回新对象（广播快照不可与内部态同体）")
	}
}
