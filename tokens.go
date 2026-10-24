// token 统计（纯函数）：固定开销 = 系统提示词 + 两条指引 + 工具 schema；历史 = 历史消息估算。
//
// 口径**复用** `data.EstimateTokens*`（字符数 / 2），不另写一套（见 41 I-21）。
package session

import (
	"encoding/json"
	"strings"

	"github.com/chonkpilot/chonkpilot-data"
)

// ToolsTokenEstimate 估算工具 schema 的 token 量（口径 = `data.EstimateTokens`）：
// 工具名 + 描述 + 参数 JSON（`json.Marshal` 键序稳定 → 可重复）。
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
	return data.EstimateTokens(b.String())
}

// HistoryTokens 估算各历史轮的 token 总量（口径 = `data.EstimateTokensOfMessages`）。
func HistoryTokens(turns [][]Message) int {
	n := 0
	for _, t := range turns {
		n += data.EstimateTokensOfMessages(toDataMessages(t))
	}
	return n
}

// FixedUsage 计算固定开销：系统提示词 + 两条指引 + 工具 schema。
func FixedUsage(systemPrompt, memoryGuide, assetGuide string, tools []ToolSchema) int {
	return data.EstimateTokens(systemPrompt) +
		data.EstimateTokens(memoryGuide) + data.EstimateTokens(assetGuide) +
		ToolsTokenEstimate(tools)
}
