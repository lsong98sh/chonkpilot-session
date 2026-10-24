// Package session 是「上下文组装 + 压缩 + 任务树」的纯 lib 组件（SESSION-1 阶段二）。
//
// 定位（依赖反置）：本包**不**感知 MQ / DB / 配置文件 / LLM 客户端——所有取数点都由宿主
// （`src/lib/llm/server`）经 `ports.go` 的端口实现；本包只做纯计算：
//
//   - turn 切分 / 轮 id 映射 / 三段边界（`turns.go`）；
//   - 三段投影（完整区原文 / 简化区仅 text / 摘要区残留同简化）与 reasoning 协议规则（`project.go`）；
//   - token 统计（固定开销 + 历史；口径 = `chonkpilot-data` 的估算函数，不另写一套，`tokens.go`）；
//   - 压缩 payload 构造 / 触发判定 / CAS 判定 / 快照消息构造（`compress.go`）；
//   - `Composer`：把上述步骤按既有顺序串起来（`compose.go`）；
//   - 任务树（建节点 / 追加 / 状态推进 / 取消级联 / 终态收敛 / 进度 / 展示映射，`tasks.go`）
//     与 `task_*` 工具的入参校验 / 结果构造（`task_tools.go`）。
//
// 行为必须与搬出前在 `src/lib/llm/server` 内的实现**逐字节等价**（system 位置、消息顺序、
// 指引顺序、reasoning 清理、tool 消息处理）。
package session

import (
	chat "github.com/lsong98sh/chonkpilot-chat"
)

// Message 是组装 / 压缩处理的消息类型。
//
// 定义为 `chat.Message` 的**别名**（不是新结构）：宿主内部一律使用 chat 组件自持类型，
// 别名保证组件与宿主之间零转换、逐字节同一（含 ID / SessionID 等宿主持有但本组件不读的字段）。
type Message = chat.Message

// ToolCall 是工具调用（`chat.ToolCall` 别名，同上）。
type ToolCall = chat.ToolCall

// Assembly 是一次 LLM 请求的组装结果。
type Assembly struct {
	// Context 发给 LLM 的全部内容（SystemPrompt + Messages）。
	Context chat.Context
	// LastTurnID 最后一个「被完整纳入」的 turn id —— 压缩回填锚点（不含当前进行中的 turn）。
	LastTurnID string
	// Zones 三段边界（均用 turn id 表达）。
	Zones ZoneIDs
	// Usage 固定开销 + 历史（阈值判定用）。
	Usage TokenUsage
}

// ZoneIDs 是三段上下文的**轮次边界**（升序：摘要区 < 简化区 < 完整区）：
//
//	完整区 = [FullFrom, …]（原文）；
//	简化区 = [BriefFrom, FullFrom)（仅 text 投影）；
//	摘要区 = 更早（已压缩为摘要）。
//
// 空串 = 该段为空 / 无法定位（如首轮：无历史轮）。
type ZoneIDs struct {
	FullFrom   string // 完整区起点 turn id
	BriefFrom  string // 简化区起点 turn id
	LastTurnID string // 最后一个纳入消息的已完成轮 id（= Assembly.LastTurnID）
}

// TokenUsage 是组装结果的 token 估算（口径 = `data.EstimateTokens*`：字符数 / 2）。
type TokenUsage struct {
	Fixed   int // 系统提示词 + 两条指引 + 工具 schema
	History int // 历史消息（不含当前进行中的 turn）
	Total   int // Fixed + History
}

// TurnToken 是一轮的**预存** token（`data-session-context` 的 turn_tokens 伴随数组元素）。
// Full / Brief 为 nil = 该轮无预存值（判定时回退实时估算，见 `data.ResolveStoredTokens`）。
type TurnToken struct {
	TurnID string
	Full   *int
	Brief  *int
}

// ToolSchema 是工具定义的**最小投影**（token 估算用；口径字段与宿主工具定义一致）。
type ToolSchema struct {
	Name        string
	Description string
	Parameters  any
}

// History 是组装所需的历史取数结果（宿主一次取齐）。
type History struct {
	// Messages 会话历史（快照前缀 + 未压缩轮；不含当前进行中的 turn），升序。
	Messages []Message
	// TurnTokens 会话各轮预存 token（排除当前轮，升序、可短可长）。
	TurnTokens []TurnToken
	// TurnMessages 当前进行中的 turn 已落库消息（升序）。
	TurnMessages []Message
}

// Result 是一次组装的结果（`Assembly` + 压缩回填所需的**内部明细**）。
//   - turnMsgs 与三段分组一一对应（升序）；
//   - histTurns = 前多少轮属于**历史**（不含当前进行中的 turn）；
//   - retainFrom = 压缩保留段起点（分组下标；< 0 = 无保留段）。
type Result struct {
	Assembly
	turnMsgs   [][]Message
	histTurns  int
	retainFrom int
}

// HistTurns 返回属于历史（不含当前进行中的 turn）的轮数（诊断 / 测试用）。
func (r Result) HistTurns() int { return r.histTurns }

// RetainFrom 返回压缩保留段起点（分组下标；< 0 = 无保留段；诊断 / 测试用）。
func (r Result) RetainFrom() int { return r.retainFrom }

// RetainedMessages 返回压缩保留段消息（简化区 + 完整区中的**历史轮**；当前轮不参与）。
func (r Result) RetainedMessages() []Message {
	if r.retainFrom < 0 || r.retainFrom >= r.histTurns {
		return nil
	}
	var out []Message
	for _, t := range r.turnMsgs[r.retainFrom:r.histTurns] {
		out = append(out, t...)
	}
	return out
}
