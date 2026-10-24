// token 估算与统计（纯函数，组件内唯一口径：字符数 / TokenDivisor）。
//
// 2026-10-24（Persist Provider 改造）：口径实现**内化到本组件**——组件不再依赖
// `chonkpilot-data`；对外保持逐字节等价（除数 2、内容 = 正文 + 工具调用名与参数）。
package session

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/lsong98sh/chonkpilot-session/store"
)

// TokenDivisor 是估算口径除数：token 数 ≈ 字符数 / TokenDivisor。
const TokenDivisor = 2

// EstimateTokens 粗略估算文本 token 数（字符数 / TokenDivisor，非精确计数）。
func EstimateTokens(text string) int {
	return utf8.RuneCountInString(text) / TokenDivisor
}

// estimateTokensOf 按同一口径累加一组消息（正文 + 工具调用名与参数）。
func estimateTokensOf[T any](msgs []T, visit func(T, func(string))) int {
	n := 0
	add := func(s string) { n += utf8.RuneCountInString(s) }
	for _, m := range msgs {
		visit(m, add)
	}
	return n / TokenDivisor
}

// EstimateTokensOfMessages 估算会话消息 token 总量（口径同上）。
// 内容含正文 + 工具调用名与参数。
func EstimateTokensOfMessages(msgs []store.ChatMsg) int {
	return estimateTokensOf(msgs, func(m store.ChatMsg, add func(string)) {
		add(m.Content)
		for _, tc := range m.ToolCalls {
			add(tc.Function.Name)
			add(tc.Function.Arguments)
		}
	})
}

// ToolsTokenEstimate 估算工具 schema 的 token 量（工具名 + 描述 + 参数 JSON）。
func ToolsTokenEstimate(tools []ToolSchema) int {
	if len(tools) == 0 {
		return 0
	}
	var b strings.Builder
	for _, t := range tools {
		b.WriteString(t.Name)
		b.WriteString("\n")
		b.WriteString(t.Description)
		b.WriteString("\n")
		if t.Parameters != nil {
			if raw, err := json.Marshal(t.Parameters); err == nil {
				b.Write(raw)
				b.WriteString("\n")
			}
		}
	}
	return EstimateTokens(b.String())
}

// HistoryTokens 估算各历史轮的 token 总量。
func HistoryTokens(turns [][]Message) int {
	n := 0
	for _, t := range turns {
		n += EstimateTokensOfMessages(toDataMessages(t))
	}
	return n
}

// FixedUsage 计算固定开销：系统提示词 + 两条指引 + 工具 schema。
func FixedUsage(systemPrompt, memoryGuide, assetGuide string, tools []ToolSchema) int {
	return EstimateTokens(systemPrompt) +
		EstimateTokens(memoryGuide) + EstimateTokens(assetGuide) +
		ToolsTokenEstimate(tools)
}
