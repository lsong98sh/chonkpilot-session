// 三段投影 + reasoning 协议规则（纯函数；搬自 `src/lib/llm/server/assemble.go`，行为逐字不变）。
//
// 三段结构（2026-09-25 口径 X）：
//   - 完整区（最近 `keepFullTurns` 轮 / `keepFullTokens` 内，**本轮恒保留**）→ 该 turn 的
//     全部消息原文（用户提问 / 中间文本 / 带 tool_calls 的 assistant / role=tool 结果 / 思考）；
//   - 简化区（再往前，brief token 累计 <= `briefBudget`）→ **【简化态原文】仅 text**
//     （`BriefMessages`：去 reasoning / tool_call / tool_result）；
//   - 摘要区（更早）→ 已由压缩落为前导 system 摘要（原样透传）；若仍有残留（压缩未及）→ 同按简化态投影。
//   - 被重试 / 恢复的 turn → 强制全量（forceFullLast）。
//
// 三段边界算法唯一来源 = `LocateZones`（**与压缩侧同一函数**）；token 估算唯一来源 =
// `EstimateTokensOfMessages`（字符数 / 2）。两者均已内化到本组件（不依赖 data）。
package session

import "github.com/lsong98sh/chonkpilot-session/store"

// Project 按 turn 分组拼接（**三段结构**），另返回本次投影使用的三段计数
// （`LocateZones` 原样带回；组装结果 `Assembly.Zones` 据此换算 turn id，避免二次定位漂移）。
//
// storedFull/storedBrief = 各轮**预存** token（随 turn_tokens 伴随数组带回；升序、可短可长，
// 按**尾部对齐**，缺值回退实时估算，见 `ResolveStoredTokens`）。
// 只返回新切片（不修改入参元素），保证历史 / 快照不被污染。
func Project(msgs []Message, keepFullTurns, keepFullTokens, briefBudget int, storedFull, storedBrief []int, forceFullLast bool) ([]Message, store.Zones) {
	if len(msgs) == 0 {
		return msgs, store.Zones{}
	}
	// 以首个轮边界把前导（system 摘要等）与各 turn 切开；无用户提问 → 原样透传。
	first := FirstTurnBoundary(msgs)
	if first < 0 {
		return msgs, store.Zones{}
	}
	turns := SplitTurns(msgs[first:])
	// 各轮完整态 / 简化态 token（简化态构造 = BriefMessages，与压缩侧/预存同源）→ 预存优先。
	fullTokens := make([]int, len(turns))
	briefTokens := make([]int, len(turns))
	for i, t := range turns {
		raw := toDataMessages(t)
		fullTokens[i] = EstimateTokensOfMessages(raw)
		briefTokens[i] = EstimateTokensOfMessages(BriefMessages(raw))
	}
	fullTokens = ResolveStoredTokens(storedFull, fullTokens)
	briefTokens = ResolveStoredTokens(storedBrief, briefTokens)
	z := LocateZones(fullTokens, briefTokens, keepFullTurns, keepFullTokens, briefBudget)
	fullStart := len(turns) - z.FullTurns
	out := make([]Message, 0, len(msgs))
	out = append(out, msgs[:first]...)
	for i, t := range turns {
		if i >= fullStart || (forceFullLast && i == len(turns)-1) {
			out = append(out, t...) // 完整区：原文
			continue
		}
		// 简化区 / 摘要区残留：仅 text（去 reasoning / tool_call / tool_result）。
		out = append(out, fromDataMessages(BriefMessages(toDataMessages(t)))...)
	}
	return out, z
}

// TurnTokenCounts 计算一轮的**完整态**与**简化态** token 数（轮次结束时预存）。
//   - 完整态 = 该轮全部消息（与组装侧 `Project` 同一估算源）；
//   - 简化态 = **仅 text**（`BriefMessages`）——与组装侧简化区投影、压缩侧预算同一构造。
func TurnTokenCounts(turn []Message) (full, brief int) {
	raw := toDataMessages(turn)
	full = EstimateTokensOfMessages(raw)
	brief = EstimateTokensOfMessages(BriefMessages(raw))
	return full, brief
}

// ApplyReasoningRule 按 DeepSeek 协议规则决定思维链回传（对最终请求消息列表生效）：
//   - 带 tool_calls 的 assistant：reasoning 必须回传（后续所有轮次，线格式 reasoning_content）；
//   - 不带 tool_calls 的 assistant（及 tool/user/system）：不回传（协议会忽略，传了也无效）。
//
// 只返回新切片（不修改入参元素），保证历史 / 快照不被污染。
func ApplyReasoningRule(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if out[i].Role != "assistant" || len(out[i].ToolCalls) == 0 {
			out[i].Reasoning = ""
		}
	}
	return out
}
