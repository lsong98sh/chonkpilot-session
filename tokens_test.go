// token 统计与工具 schema 估算的 L1 直测（期望值搬自 `src/lib/llm/server/assembly_test.go`）。
package session

import (
	"strings"
	"testing"

	"github.com/lsong98sh/chonkpilot-session/store"
)

// TestToolsTokenEstimate：工具 schema（名 + 描述 + 参数 JSON）计入固定开销。
func TestToolsTokenEstimate(t *testing.T) {
	if got := ToolsTokenEstimate(nil); got != 0 {
		t.Fatalf("无工具 → 0，got %d", got)
	}
	one := ToolsTokenEstimate([]ToolSchema{{
		Name:        "core_file_read",
		Description: "读取文件内容（按路径）",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}})
	if one <= 0 {
		t.Fatalf("单个工具 schema 估算应 > 0，got %d", one)
	}
	two := ToolsTokenEstimate([]ToolSchema{
		{Name: "a", Description: strings.Repeat("描", 40)},
		{Name: "b", Description: strings.Repeat("述", 40)},
	})
	if two <= one {
		t.Fatalf("两个工具估算应大于一个: one=%d two=%d", one, two)
	}
}

// TestFixedAndHistoryTokens：固定开销 = 系统提示词 + 两条指引 + 工具 schema；
// 历史 = 各轮消息估算（口径 = EstimateTokens*）。
func TestFixedAndHistoryTokens(t *testing.T) {
	sys, mem, asset := "系统提示词层", "记忆指引", "资产指引"
	tools := []ToolSchema{{Name: "t", Description: "说明"}}
	want := EstimateTokens(sys) + EstimateTokens(mem) + EstimateTokens(asset) +
		ToolsTokenEstimate(tools)
	if got := FixedUsage(sys, mem, asset, tools); got != want {
		t.Fatalf("FixedUsage = %d, want %d", got, want)
	}
	turns := [][]Message{
		{{Role: "user", Kind: "text", Content: "q1"}, {Role: "assistant", Content: "a1"}},
	}
	wantHist := EstimateTokensOfMessages([]store.ChatMsg{
		{Role: "user", Kind: "text", Content: "q1"}, {Role: "assistant", Content: "a1"},
	})
	if got := HistoryTokens(turns); got != wantHist {
		t.Fatalf("HistoryTokens = %d, want %d", got, wantHist)
	}
	if got := HistoryTokens(nil); got != 0 {
		t.Fatalf("无历史轮 → 0，got %d", got)
	}
}
