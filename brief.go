// brief.go —— 【简化态原文】构造（口径 X ②，2026-09-25；唯一实现）。
//
// 规则：role=tool 整条丢弃；role=assistant 仅保留正文（去 reasoning / tool_calls，正文空白则丢弃）；
// 其余（user / system）原样保留。只返回新切片（不修改入参元素）。
//
// 2026-10-24（Persist Provider 改造）：自 `chonkpilot-data` 内化到本组件；门面 DTO 版
// （`BriefFacadeMessages`）留在 data 侧，规则同源（同一实现思路）。
package session

import (
	"strings"

	"github.com/lsong98sh/chonkpilot-session/store"
)

// BriefMessages 构造一段消息的【简化态原文】（内核 `store.ChatMsg`；组装侧用）。
func BriefMessages(msgs []store.ChatMsg) []store.ChatMsg {
	out := make([]store.ChatMsg, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			continue
		case "assistant":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			m.Reasoning = ""
			m.ToolCalls = nil
			out = append(out, m)
		default:
			out = append(out, m)
		}
	}
	return out
}
