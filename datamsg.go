// chat 消息与快照契约消息的最小互转（本组件**唯一**类型转换点）。
//
// 2026-10-24（Persist Provider 改造）：契约类型已内化到 `store.ChatMsg / store.ToolCall`
// （与既有数据面 JSON 逐字段一致），组件不再 import `chonkpilot-data`。
// 形状与宿主 `src/lib/llm/server/chatadapter.go` 的 `ChatMessageToData` / `ChatMessageFromData`
// 逐字段一致（Type 固定 function）。
package session

import "github.com/chonkpilot/chonkpilot-session/store"

// toolCallTypeFunction 是快照 ToolCall.type 的固定取值（OpenAI 兼容形态）。
const toolCallTypeFunction = "function"

// toDataMessage 把 chat 消息折成快照契约消息（落库形态）。
func toDataMessage(m Message) store.ChatMsg {
	out := store.ChatMsg{
		Role:       m.Role,
		Kind:       m.Kind,
		Content:    m.Content,
		Reasoning:  m.Reasoning,
		ToolCallID: m.ToolCallID,
		Meta:       m.Meta,
	}
	if len(m.ToolCalls) > 0 {
		out.ToolCalls = make([]store.ToolCall, 0, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			var tc store.ToolCall
			tc.ID = c.ID
			tc.Type = toolCallTypeFunction
			tc.Function.Name = c.Name
			tc.Function.Arguments = c.Arguments
			out.ToolCalls = append(out.ToolCalls, tc)
		}
	}
	return out
}

// fromDataMessage 把快照契约消息还原为 chat 消息（ToolCall.type 丢弃，chat 形态无该字段）。
func fromDataMessage(m store.ChatMsg) Message {
	out := Message{
		Role:       m.Role,
		Kind:       m.Kind,
		Content:    m.Content,
		Reasoning:  m.Reasoning,
		ToolCallID: m.ToolCallID,
		Meta:       m.Meta,
	}
	if len(m.ToolCalls) > 0 {
		out.ToolCalls = make([]ToolCall, 0, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments,
			})
		}
	}
	return out
}

// toDataMessages 批量转换（nil 安全：空入参 → nil）。
func toDataMessages(msgs []Message) []store.ChatMsg {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]store.ChatMsg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toDataMessage(m))
	}
	return out
}

// fromDataMessages 批量转换（nil 安全：空入参 → nil）。
func fromDataMessages(msgs []store.ChatMsg) []Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, fromDataMessage(m))
	}
	return out
}
