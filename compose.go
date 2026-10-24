// Composer：把「历史取数 → 三段投影 → reasoning 规则 → 轮分组 / 边界 → token 统计 →
// 两条指引前置」按既有顺序串起来（纯计算，全部取数经 ports.go 的端口）。
//
// 逐字节等价的硬约束（与搬出前 `turnCtx.buildAssembly` 一致）：
//  1. 消息顺序 = 历史（含前导 system / 摘要）+ 当前 turn 已落库消息，经三段投影后保持原序；
//  2. reasoning 规则在**投影后**应用（仅带 tool_calls 的 assistant 回传）；
//  3. 两条指引 prepend 到最前：先记忆后资产 → 最终 `[资产, 记忆, ...]`；
//  4. 轮分组 / 三段边界 / token 统计都在**插入指引之前**的消息上计算；
//  5. 指引正文为空 → 不插入该条。
package session

import (
	"context"

	chat "github.com/lsong98sh/chonkpilot-chat"
)

// Request 是一次组装的轮次身份（不含任何取数）。
type Request struct {
	// CurrentTurnID 当前进行中的 turn id（轮 id 映射的当前轮取值）。
	CurrentTurnID string
	// ForceFull 重试 / 恢复的 turn → 最后一段强制全量。
	ForceFull bool
}

// Composer 是上下文装配器（无状态；端口由宿主注入）。
type Composer struct {
	History HistorySource
	Layers  LayerSource
	Guides  GuideSource
	Policy  PolicySource
	Tools   ToolSource
}

// Build 组装本轮上下文并返回 `Assembly` + 压缩回填所需的内部明细。
func (c *Composer) Build(ctx context.Context, req Request) Result {
	h, _ := c.History.Load(ctx) // 读失败已由宿主按空历史处置（不猜）
	all := append(append([]Message{}, h.Messages...), h.TurnMessages...)

	storedFull, storedBrief := SplitTurnTokens(h.TurnTokens)
	msgs, zones := Project(all,
		c.Policy.KeepFullTurns(ctx), c.Policy.KeepFullTokens(ctx), c.Policy.BriefBudget(ctx),
		storedFull, storedBrief, req.ForceFull)
	msgs = ApplyReasoningRule(msgs)

	// 指引只读一次由宿主保证（端口实现内含单轮缓存）；拼接顺序与搬出前一致：
	// 先读记忆、后读资产，先后 prepend → 最终 `[资产, 记忆, ...]`。
	memGuide := c.Guides.Memory(ctx)
	assetGuide := c.Guides.Asset(ctx)

	out := Result{Assembly: Assembly{Context: chat.Context{Messages: msgs}}}
	// 三段分组（**不含**即将插入的两条指引：指引不是历史轮，也不参与轮边界）。
	turns, ids, histTurns := TurnGroups(msgs, h.Messages, h.TurnTokens, req.CurrentTurnID)
	out.turnMsgs, out.histTurns = turns, histTurns
	// 三段边界（turn id 口径）+ 压缩保留段起点 + 最后一个已完成轮。
	out.Zones = ZoneIDsOf(ids, zones)
	out.retainFrom = RetainStartIndex(ids, zones, histTurns)
	if histTurns > 0 {
		out.LastTurnID = ids[histTurns-1]
	}
	out.Zones.LastTurnID = out.LastTurnID
	// 用量：固定开销（system prompt + 两条指引 + 工具 schema）+ **历史轮**消息。
	fixed := FixedUsage(c.Layers.SystemPrompt(ctx), memGuide, assetGuide, c.Tools.Tools(ctx))
	history := HistoryTokens(turns[:histTurns])
	out.Usage = TokenUsage{Fixed: fixed, History: history, Total: fixed + history}
	// 指引插到最前（与搬出前一致）：先记忆后资产。
	if memGuide != "" {
		msgs = append([]Message{{Role: "system", Content: memGuide}}, msgs...)
	}
	if assetGuide != "" {
		msgs = append([]Message{{Role: "system", Content: assetGuide}}, msgs...)
	}
	out.Context.Messages = msgs
	return out
}
