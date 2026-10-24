// chat 消息与 data 内核消息的最小互转（本组件**唯一**类型转换点）。
//
// 存在理由：token 估算与【简化态原文】构造的唯一实现都在 `chonkpilot-data`
// （`EstimateTokensOfMessages` / `BriefMessages` / `LocateZones`），本组件必须按 data 内核类型
// 调用它们——绝不另写一套口径。形状与宿主 `src/lib/llm/server/chatadapter.go` 的
// `ChatMessageToData` / `ChatMessageFromData` 逐字段一致（Type 固定 function）。
package session

import "github.com/chonkpilot/chonkpilot-data"

// toolCallTypeFunction 是快照 ToolCall.type 的固定取值（OpenAI 兼容形态，data 侧既有口径）。
const toolCallTypeFunction = "function"

// toDataMessage 把 chat 消息折成 data 内核消息（快照 / 落库形态）。
func toDataMessage(m Message) data.ChatMsg {
	out := data.ChatMsg{
		Role:       m.Role,
		Kind:       m.Kind,
		Content:    m.Content,
		Reasoning:  m.Reasoning,
		ToolCallID: m.ToolCallID,
		Meta:       m.Meta,
	}
	if len(m.ToolCalls) > 0 {
		out.ToolCalls = make([]data.ToolCall, 0, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			var tc data.ToolCall
			tc.ID = c.ID
			tc.Type = toolCallTypeFunction
			tc.Function.Name = c.Name
			tc.Function.Arguments = c.Arguments
			out.ToolCalls = append(out.ToolCalls, tc)
		}
	}
	return out
}

// fromDataMessage 把 data 内核消息还原为 chat 消息（ToolCall.type 丢弃，chat 形态无该字段）。
func fromDataMessage(m data.ChatMsg) Message {
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
func toDataMessages(msgs []Message) []data.ChatMsg {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]data.ChatMsg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toDataMessage(m))
	}
	return out
}

// fromDataMessages 批量转换（nil 安全：空入参 → nil）。
func fromDataMessages(msgs []data.ChatMsg) []Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, fromDataMessage(m))
	}
	return out
}
