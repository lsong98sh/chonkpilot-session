// task 型域工具（tool_stop / tool_result）的**纯语义映射**：入参校验 + 结果文案构造。
//
// 边界：只做「LLM 工具入参 → 内部请求」与「内部状态/计数 → 结果文本」的映射，**不含**任何
// 副作用——任务层读（State / ExecOf）、执行侧取消、message 表读仍由宿主完成。
// 文案 / 错误位必须与搬出前 `src/lib/llm/server/tool_task.go` 逐字节等价。
package session

import "fmt"

// ToolStopRequest 是 tool_stop 的入参（校验后）。
type ToolStopRequest struct {
	TaskID    string
	ProcessID int
}

// ParseToolStopArgs 校验 tool_stop 入参（**顺序即契约：process_id 先于 task_id**）。
//
// process_id 参数（B-01 安全）：**一律拒绝**——宿主不持有任何子进程 PID 登记面，无法校验
// PID 归属，直接拒绝，杜绝 LLM 经任意 PID 击杀本机无关进程。终止任务请改用 task_id
// （级联取消，含转后台执行体）。
//
// 返回 (请求, 错误文案, isErr)；isErr=true 时错误文案即工具返回文本。
func ParseToolStopArgs(args map[string]any) (ToolStopRequest, string, bool) {
	processID, _ := args["process_id"].(float64)
	if processID > 0 {
		return ToolStopRequest{}, fmt.Sprintf("错误: 不支持按 process_id(%d) 终止进程（本服务不登记子进程 PID，无法校验归属）；请改用 task_id 级联取消", int(processID)), true
	}
	taskID, _ := args["task_id"].(string)
	if taskID == "" {
		return ToolStopRequest{}, "错误: task_id 必填（process_id 已不支持）", true
	}
	return ToolStopRequest{TaskID: taskID, ProcessID: int(processID)}, "", false
}

// ToolStopAlreadyDoneText 已是不可逆终态时的返回文本（isErr=false）。
func ToolStopAlreadyDoneText(taskID, state string) string {
	return fmt.Sprintf("任务 %s 已结束（%s），无需停止", taskID, state)
}

// ToolStopExecUnavailableText 层持执行句柄但执行池未注入时的返回文本（isErr=true）。
func ToolStopExecUnavailableText(taskID string) string {
	return fmt.Sprintf("错误: 执行池不可用（未注入 sink），无法打断任务 %s", taskID)
}

// ToolStopNotFoundText 任务不存在或层/内存均判无事可取消时的返回文本（isErr=false）。
func ToolStopNotFoundText(taskID string) string {
	return fmt.Sprintf("任务 %s 不存在或已结束", taskID)
}

// ToolStopOKText 级联取消成功时的返回文本（isErr=false）。
func ToolStopOKText(taskID string, cancelled int) string {
	return fmt.Sprintf("🛑 任务 %s 已级联停止（取消 %d 个节点）", taskID, cancelled)
}

// ToolResultRequest 是 tool_result 的入参（校验后）。
type ToolResultRequest struct {
	ID string
	// TimeoutSec 等待秒数（0 = 立即查询一次；调用方据此换算 deadline）。
	TimeoutSec float64
}

// ParseToolResultArgs 校验 tool_result 入参：id 必填；timeout 缺省 30s，<=0 收敛为 0
// （= 立即查询一次，不取消被查询任务）。
func ParseToolResultArgs(args map[string]any) (ToolResultRequest, string, bool) {
	id, _ := args["id"].(string)
	if id == "" {
		return ToolResultRequest{}, "错误: id 必填", true
	}
	timeout := 30.0
	if v, ok := args["timeout"].(float64); ok {
		timeout = v
	}
	if timeout <= 0 {
		timeout = 0 // 0/负 → 立即查询一次
	}
	return ToolResultRequest{ID: id, TimeoutSec: timeout}, "", false
}

// ToolResultCancelledText 查询执行体被取消时的返回文本（isErr=false；被查询任务本身未受影响）。
func ToolResultCancelledText(id string) string {
	return fmt.Sprintf("已取消: 任务 %s 查询中止（gateway 执行侧取消；被查询任务本身未受影响，可重新调用 tool_result）", id)
}

// ToolResultRunningText 等待超时仍在运行时的返回文本（isErr=false；不取消任务）。
func ToolResultRunningText(id string, timeoutSec float64) string {
	return fmt.Sprintf("任务尚未结束：%s 仍在运行（已等待 %.0fs）。可再次调用 tool_result(id=%q, timeout=更长秒数) 继续等待", id, timeoutSec, id)
}

// ToolResultNotFoundText 未找到任务时的返回文本（isErr=true）。
func ToolResultNotFoundText(id string) string {
	return "错误: 未找到任务 " + id
}

// AsyncResultUnavailableText = 层判终态但 message 表无该调用的结果行（明确文案，不静默返回空）。
const AsyncResultUnavailableText = "异步结果已不可用（任务已终态，但 message 表无该调用的结果行）"

// TaskNodeResultText 由**已终态**节点给出查询结果：(文本, 状态, 是否命中)。
// 非终态 / 节点为空 → ok=false（调用方继续按层状态与 message 表判定）。
func TaskNodeResultText(n *TaskNode) (text, state string, ok bool) {
	if n == nil {
		return "", "", false
	}
	switch n.State {
	case TaskStateDone:
		return n.ResultSummary, "done", true
	case TaskStateError:
		return "任务失败: " + n.Error, "error", true
	case TaskStateCancelled:
		return "任务已取消: " + n.Error, "cancelled", true
	}
	return "", "", false
}
