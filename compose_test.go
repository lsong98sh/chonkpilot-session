// Composer 装配的 L1 直测（端口全部用假实现）：消息顺序 / 指引顺序 / 三段边界 /
// 轮 id 映射 / token 统计，与搬出前 `turnCtx.buildAssembly` 的期望逐项对齐。
package session

import (
	"context"
	"reflect"
	"testing"

	chat "github.com/lsong98sh/chonkpilot-chat"
)

type fakeHistory struct{ h History }

func (f fakeHistory) Load(context.Context) (History, error) { return f.h, nil }

type fakeLayers struct{ text string }

func (f fakeLayers) SystemPrompt(context.Context) string { return f.text }

type fakeGuides struct {
	mem, asset       string
	memCalls, assetC int
}

func (f *fakeGuides) Memory(context.Context) string { f.memCalls++; return f.mem }
func (f *fakeGuides) Asset(context.Context) string  { f.assetC++; return f.asset }

type fakePolicy struct{ turns, tokens, budget int }

func (f fakePolicy) KeepFullTurns(context.Context) int  { return f.turns }
func (f fakePolicy) KeepFullTokens(context.Context) int { return f.tokens }
func (f fakePolicy) BriefBudget(context.Context) int    { return f.budget }

type fakeTools struct{ tools []ToolSchema }

func (f fakeTools) Tools(context.Context) []ToolSchema { return f.tools }

func oneInt(v int) *int { return &v }

// TestComposerBuildOrderAndZones：`[资产, 记忆, system, 历史轮…, 本轮…]`；
// reasoning 规则生效；Zones / LastTurnID / Usage 口径与搬出前一致。
func TestComposerBuildOrderAndZones(t *testing.T) {
	tools := []ToolSchema{{Name: "core_file_read", Description: "读文件"}}
	guides := &fakeGuides{mem: "【记忆】M", asset: "【知识库资产】A"}
	c := &Composer{
		History: fakeHistory{h: History{
			Messages: []Message{
				{Role: "system", Content: "SYS"},
				{Role: "user", Kind: "text", Content: "q1"},
				{Role: "assistant", Content: "a1", Reasoning: "旧思考"},
			},
			TurnTokens: []TurnToken{{TurnID: "t1"}},
			TurnMessages: []Message{
				{Role: "user", Kind: "text", Content: "q2"},
				{Role: "assistant", Content: "", ToolCalls: []ToolCall{mkToolCall("c1", "read", "{}")}, Reasoning: "想"},
				{Role: "tool", ToolCallID: "c1", Content: "r"},
			},
		}},
		Layers: fakeLayers{text: "SYS"},
		Guides: guides,
		Policy: fakePolicy{turns: 2, budget: 10000},
		Tools:  fakeTools{tools: tools},
	}
	res := c.Build(context.Background(), Request{CurrentTurnID: "t-cur"})

	want := []Message{
		{Role: "system", Content: "【知识库资产】A"},
		{Role: "system", Content: "【记忆】M"},
		{Role: "system", Content: "SYS"},
		{Role: "user", Kind: "text", Content: "q1"},
		{Role: "assistant", Content: "a1"}, // reasoning 被清（无 tool_calls）
		{Role: "user", Kind: "text", Content: "q2"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("c1", "read", "{}")}, Reasoning: "想"},
		{Role: "tool", ToolCallID: "c1", Content: "r"},
	}
	if !reflect.DeepEqual(res.Context.Messages, want) {
		t.Fatalf("消息顺序 / 指引顺序不符:\n got=%+v\nwant=%+v", res.Context.Messages, want)
	}
	if res.HistTurns() != 1 {
		t.Fatalf("历史轮数应为 1，got %d", res.HistTurns())
	}
	if res.LastTurnID != "t1" || res.Zones.LastTurnID != "t1" {
		t.Fatalf("LastTurnID 不符: LastTurnID=%q Zones.LastTurnID=%q", res.LastTurnID, res.Zones.LastTurnID)
	}
	if res.Zones.FullFrom != "t1" || res.Zones.BriefFrom != "" {
		t.Fatalf("三段边界不符（2 轮全在完整区）: %+v", res.Zones)
	}
	wantFixed := EstimateTokens("SYS") + EstimateTokens("【记忆】M") +
		EstimateTokens("【知识库资产】A") + ToolsTokenEstimate(tools)
	if res.Usage.Fixed != wantFixed || res.Usage.Total != res.Usage.Fixed+res.Usage.History {
		t.Fatalf("Usage 口径不符: %+v (want Fixed=%d)", res.Usage, wantFixed)
	}
	// 历史 token 只算历史轮（不含本轮工具轮）。
	wantHist := EstimateTokensOfMessages(toDataMessages(want[3:5]))
	if res.Usage.History != wantHist {
		t.Fatalf("Usage.History = %d, want %d", res.Usage.History, wantHist)
	}
	// 保留段 = 简化区 + 完整区中的历史轮（本轮不参与）。
	retained := res.RetainedMessages()
	if len(retained) != 2 || retained[0].Content != "q1" {
		t.Fatalf("保留段应为历史轮原文: %+v", retained)
	}
	// 指引各读一次（组件不缓存，宿主负责单轮缓存）。
	if guides.memCalls != 1 || guides.assetC != 1 {
		t.Fatalf("指引读取次数: mem=%d asset=%d", guides.memCalls, guides.assetC)
	}
}

// TestComposerEmptyGuides：指引为空 → 不插入（与搬出前一致）。
func TestComposerEmptyGuides(t *testing.T) {
	c := &Composer{
		History: fakeHistory{h: History{Messages: []Message{
			{Role: "user", Kind: "text", Content: "q1"},
		}}},
		Layers: fakeLayers{},
		Guides: &fakeGuides{},
		Policy: fakePolicy{turns: 1, budget: 10000},
		Tools:  fakeTools{},
	}
	res := c.Build(context.Background(), Request{CurrentTurnID: "t1"})
	want := []Message{{Role: "user", Kind: "text", Content: "q1"}}
	if !reflect.DeepEqual(res.Context.Messages, want) {
		t.Fatalf("空指引不应插入: %+v", res.Context.Messages)
	}
	if res.LastTurnID != "" {
		t.Fatalf("无历史轮 → LastTurnID 应为空，got %q", res.LastTurnID)
	}
	if res.Usage.Fixed != 0 {
		t.Fatalf("空配置下固定开销应为 0: %+v", res.Usage)
	}
	if wantHist := EstimateTokensOfMessages(toDataMessages(want)); res.Usage.History != wantHist {
		t.Fatalf("Usage.History = %d, want %d", res.Usage.History, wantHist)
	}
}

// TestComposerForceFull：ForceFull（重试 / 恢复轮）→ 最后一段强制全量。
func TestComposerForceFull(t *testing.T) {
	hist := twoTurnHist()[1:] // T1 + T2（无前导 system）
	c := &Composer{
		History: fakeHistory{h: History{Messages: hist, TurnTokens: []TurnToken{
			{TurnID: "t1", Full: oneInt(1000)}, {TurnID: "t2", Full: oneInt(1000)},
		}}},
		Layers: fakeLayers{},
		Guides: &fakeGuides{},
		// 完整区只留 1 轮 + 简化预算 0 → T1 本应简化；ForceFull 只影响最后一段。
		Policy: fakePolicy{turns: 1},
		Tools:  fakeTools{},
	}
	res := c.Build(context.Background(), Request{CurrentTurnID: "t2", ForceFull: true})
	// T1 简化态（问1 / 中间文本1 / 结论1）+ T2 全量（5 条）
	if len(res.Context.Messages) != 3+5 {
		t.Fatalf("ForceFull 下最后一段应全量: %d %+v", len(res.Context.Messages), res.Context.Messages)
	}
	if res.HistTurns() != 2 {
		t.Fatalf("历史轮数应为 2（无当前轮消息），got %d", res.HistTurns())
	}
}

// TestNewJob：压缩任务 = payload 全量 + 保留段（仅历史轮）。
func TestNewJob(t *testing.T) {
	res := Result{
		Assembly:   Assembly{LastTurnID: "t1", Context: chat.Context{Messages: []Message{{Role: "system", Content: "S"}}}},
		turnMsgs:   [][]Message{{{Role: "user", Content: "q1"}}, {{Role: "user", Content: "q2"}}},
		histTurns:  1,
		retainFrom: 0,
	}
	job := NewJob(res, "ins", "s", "t2")
	if job.InstanceID != "ins" || job.SessionID != "s" || job.TurnID != "t2" || job.Anchor != "t1" {
		t.Fatalf("任务身份不符: %+v", job)
	}
	if len(job.Payload) != 1 || len(job.Retain) != 1 || job.Retain[0].Content != "q1" {
		t.Fatalf("payload / 保留段不符: %+v", job)
	}
}
