// 三段投影 / 轮边界 / reasoning 规则的 L1 直测（纯函数，无端口依赖）。
//
// 期望值自 `src/lib/llm/server/assemble_test.go` 原样搬入（行为逐字节等价的回归门槛）。
package session

import (
	"strings"
	"testing"

	"github.com/chonkpilot/chonkpilot-data"
)

// mkToolCall 造一条工具调用（参数已序列化）。
func mkToolCall(id, name, args string) ToolCall {
	return ToolCall{ID: id, Name: name, Arguments: args}
}

// twoTurnHist：system 摘要 + T1（中间文本 + 工具 + 结果 + 结论）+ T2（中间文本 + 工具 + 结果 + 结论）。
func twoTurnHist() []Message {
	return []Message{
		{Role: "system", Content: "[已压缩早前对话] 摘要"},
		// T1
		{Role: "user", Kind: "text", Content: "问1"},
		{Role: "assistant", Content: "中间文本1"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{mkToolCall("c1", "read", `{"p":"a"}`)}, Reasoning: "想1"},
		{Role: "tool", ToolCallID: "c1", Content: "结果1"},
		{Role: "assistant", Content: "结论1", Reasoning: "想1b"},
		// T2
		{Role: "user", Kind: "text", Content: "问2"},
		{Role: "assistant", Content: "中间文本2"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{mkToolCall("c2", "read", `{"p":"b"}`)}},
		{Role: "tool", ToolCallID: "c2", Content: "结果2"},
		{Role: "assistant", Content: "结论2"},
	}
}

// TestProjectKeepRecentFullRestBrief（口径 X 三段）：完整区（最近 1 轮）全量；
// 简化区（更早轮）→ **仅 text**（去 reasoning / tool_call / tool_result）；前导 system 摘要原样透传；
// 不修改入参。
func TestProjectKeepRecentFullRestBrief(t *testing.T) {
	hist := twoTurnHist()
	out, _ := Project(hist, 1, 0, 0, nil, nil, false)
	if len(out) != 1+3+5 {
		t.Fatalf("拼接条数错：got %d %+v", len(out), out)
	}
	if out[0].Role != "system" {
		t.Fatalf("前导 system 摘要应透传：%+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "问1" {
		t.Fatalf("简化区应保留用户 text：%+v", out[1])
	}
	if out[2].Role != "assistant" || out[2].Content != "中间文本1" || len(out[2].ToolCalls) != 0 {
		t.Fatalf("简化区应保留 assistant text 且去 tool_calls：%+v", out[2])
	}
	if out[3].Role != "assistant" || out[3].Content != "结论1" {
		t.Fatalf("简化区应保留收尾 text：%+v", out[3])
	}
	want := hist[6:]
	for i := range want {
		if out[4+i].Role != want[i].Role || out[4+i].Content != want[i].Content {
			t.Fatalf("完整区第 %d 条不符：got %+v want %+v", i, out[4+i], want[i])
		}
	}
	if len(out[6].ToolCalls) != 1 || out[6].ToolCalls[0].ID != "c2" {
		t.Fatalf("完整区工具调用应原文保留：%+v", out[6])
	}
	if out[7].Role != "tool" || out[7].Content != "结果2" {
		t.Fatalf("完整区工具结果应原文保留：%+v", out[7])
	}
	// 入参不被篡改
	if len(hist) != 11 || hist[2].Content != "中间文本1" || len(hist[3].ToolCalls) != 1 {
		t.Fatalf("入参被篡改：%+v", hist)
	}
}

// TestProjectMultipleToolsAndTexts：维持轮内多工具调用 + 多条中间文本全量保留。
func TestProjectMultipleToolsAndTexts(t *testing.T) {
	hist := []Message{
		{Role: "user", Kind: "text", Content: "问1"},
		{Role: "assistant", Content: "文本A"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			mkToolCall("c1", "read", `{"p":"a"}`), mkToolCall("c2", "write", `{"p":"b"}`),
		}},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "tool", ToolCallID: "c2", Content: "r2"},
		{Role: "assistant", Content: "文本B"},
		{Role: "assistant", Content: "结论"},
	}
	out, _ := Project(hist, 10, 0, 0, nil, nil, false)
	if len(out) != len(hist) {
		t.Fatalf("完整区应全量：got %d want %d %+v", len(out), len(hist), out)
	}
	if len(out[2].ToolCalls) != 2 || out[2].ToolCalls[1].ID != "c2" {
		t.Fatalf("多工具调用应逐条保留：%+v", out[2].ToolCalls)
	}
	if out[1].Content != "文本A" || out[5].Content != "文本B" {
		t.Fatalf("多条中间文本应保留：%+v", out)
	}
}

// TestProjectBriefZoneTextOnly（口径 X）：简化区（更早轮）**仅保留 text**——
// 无收尾结论、以工具调用收尾的轮 → 只留用户提问（工具调用/结果被去掉）。
func TestProjectBriefZoneTextOnly(t *testing.T) {
	hist := []Message{
		{Role: "user", Kind: "text", Content: "问1"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{mkToolCall("c1", "read", `{}`)}},
		{Role: "tool", ToolCallID: "c1", Content: "结果1"},
		{Role: "user", Kind: "text", Content: "问2"},
		{Role: "assistant", Content: "结论2"},
	}
	out, _ := Project(hist, 1, 0, 0, nil, nil, false)
	if len(out) != 3 {
		t.Fatalf("简化区应仅 text（工具调用/结果被去掉）：got %d %+v", len(out), out)
	}
	if out[0].Role != "user" || out[0].Content != "问1" {
		t.Fatalf("简化区应保留用户 text：%+v", out[:1])
	}
	if out[1].Content != "问2" || out[2].Content != "结论2" {
		t.Fatalf("完整区应全量：%+v", out[1:])
	}
}

// TestProjectForceFullLast：最新轮（当前 turn）恒全量（口径 V，共享函数保留轮数下限 1）；
// forceFullLast=true（重试/恢复的 turn）给出同一结果，保留断言防回归。
func TestProjectForceFullLast(t *testing.T) {
	hist := twoTurnHist()[1:] // 去掉 system：T1 + T2
	no, _ := Project(hist, 1, 0, 0, nil, nil, false)
	if len(no) != 3+5 || no[0].Content != "问1" || no[2].Content != "结论1" {
		t.Fatalf("最新轮应恒全量（T1 简化态 3 条 + T2 全量 5 条）：got %d %+v", len(no), no)
	}
	ff, _ := Project(hist, 1, 0, 0, nil, nil, true)
	if len(ff) != 3+5 || ff[0].Content != "问1" {
		t.Fatalf("forceFullLast 应保留最后一段全量：got %d %+v", len(ff), ff)
	}
	if ff[3].Role != "user" || ff[3].Content != "问2" {
		t.Fatalf("最后一段应从其用户提问起全量：%+v", ff[3])
	}
	if ff[6].Role != "tool" || len(ff[5].ToolCalls) != 1 {
		t.Fatalf("最后一段工具/结果应全量：%+v", ff[3:])
	}
	// 口径 W：两条件均不启用 → 不压缩 → 全部轮次原样全量。
	if all, _ := Project(hist, 0, 0, 0, nil, nil, false); len(all) != len(hist) {
		t.Fatalf("N=M=0 不压缩应保留全量：got %d want %d", len(all), len(hist))
	}
}

// TestProjectBoundaryRules：系统注入消息（Kind=notify/continue/resume）不计入轮边界；
// 用户真实提问（Kind=text，即便文本为"继续"）仍是新轮边界；无用户提问（纯 system/摘要）原样透传。
func TestProjectBoundaryRules(t *testing.T) {
	// 纯 system（无用户提问）→ 原样
	sysOnly := []Message{{Role: "system", Content: "sys"}, {Role: "assistant", Content: "x"}}
	if out, _ := Project(sysOnly, 0, 0, 0, nil, nil, false); len(out) != len(sysOnly) {
		t.Fatalf("无用户提问应原样透传：%+v", out)
	}
	// notify / 注入续写 属于当前 turn，不新开轮 → **单一轮**（组装侧下限 → 最新轮全量，整轮原文）
	hist := []Message{
		{Role: "user", Kind: "text", Content: "问1"},
		{Role: "user", Kind: KindNotify, Content: "[工具通知] n"},
		{Role: "user", Kind: KindContinue, Content: ContinueText},
		{Role: "assistant", Content: "结论1"},
	}
	out, _ := Project(hist, 0, 0, 0, nil, nil, false)
	if len(out) != len(hist) {
		t.Fatalf("notify/continue 不应作为轮边界（单轮全量）：%+v", out)
	}
	// 用户真实提问（Kind=text）文本恰为"继续" → 是轮边界：新轮全量、前轮简化态（N=1）
	real := []Message{
		{Role: "user", Kind: "text", Content: "问1"},
		{Role: "assistant", Content: "结论1"},
		{Role: "user", Kind: "text", Content: ContinueText},
		{Role: "assistant", Content: "结论2"},
	}
	ro, _ := Project(real, 1, 0, 0, nil, nil, false)
	if len(ro) != 2+2 || ro[0].Content != "问1" || ro[1].Content != "结论1" ||
		ro[2].Content != ContinueText || ro[3].Content != "结论2" {
		t.Fatalf("用户真实'继续'应为新轮边界（前轮简化 text + 新轮全量）：%+v", ro)
	}
}

// TestProjectKeepFullIncludesCurrent：keepFullTurns=N 表示"**含当前轮**的最近 N 轮全量"。
func TestProjectKeepFullIncludesCurrent(t *testing.T) {
	hist := []Message{
		{Role: "user", Kind: "text", Content: "问1"}, {Role: "assistant", Content: "结论1"},
		{Role: "user", Kind: "text", Content: "问2"}, {Role: "assistant", Content: "结论2"},
		{Role: "user", Kind: "text", Content: "问3"}, {Role: "assistant", Content: "结论3"},
	}
	out, _ := Project(hist, 2, 0, 0, nil, nil, false)
	if len(out) != 2+4 || out[0].Content != "问1" ||
		out[2].Content != "问2" || out[5].Content != "结论3" {
		t.Fatalf("keep=2 应含当前轮共 2 轮全量：got %d %+v", len(out), out)
	}
	out1, _ := Project(hist, 1, 0, 0, nil, nil, false)
	if len(out1) != 4+2 || out1[0].Content != "问1" || out1[2].Content != "问2" || out1[4].Content != "问3" {
		t.Fatalf("keep=1 应仅当前轮全量：got %d %+v", len(out1), out1)
	}
}

// TestProjectTokenBound（口径 X）：完整区受 `keepFullTokens`(M) 约束（自最新轮向前累计完整态
// token）；更早轮 → **简化态（仅 text）**；N 足够大时以 M 为准；x=0 → 下限仍保最新轮全量。
func TestProjectTokenBound(t *testing.T) {
	big := strings.Repeat("x", 200) // 200 字符正文
	turn := func(q string) []Message {
		return []Message{
			{Role: "user", Kind: "text", Content: q},
			{Role: "assistant", Content: big, ToolCalls: []ToolCall{mkToolCall("c-"+q, "read", `{}`)}},
			{Role: "tool", ToolCallID: "c-" + q, Content: big},
		}
	}
	var hist []Message
	for _, q := range []string{"q1", "q2", "q3"} {
		hist = append(hist, turn(q)...)
	}
	out, _ := Project(hist, 10, 400, 0, nil, nil, false)
	if len(out) != 2+2+3 {
		t.Fatalf("超 M → 前 2 轮简化态、末轮全量：got %d %+v", len(out), out)
	}
	if out[0].Content != "q1" || out[1].Content != big || len(out[1].ToolCalls) != 0 {
		t.Fatalf("简化态 = 仅 text（去工具调用）：%+v", out[:2])
	}
	if out[4].Content != "q3" || len(out[5].ToolCalls) != 1 || out[6].Role != "tool" {
		t.Fatalf("完整区末轮应含工具调用与结果原文：%+v", out[4:])
	}
	if all, _ := Project(hist, 10, 100000, 0, nil, nil, false); len(all) != len(hist) {
		t.Fatalf("M 未超 → 全量：got %d %+v", len(all), all)
	}
	z, _ := Project(hist, 10, 1, 0, nil, nil, false)
	if len(z) != 2+2+3 || z[4].Content != "q3" || len(z[5].ToolCalls) != 1 {
		t.Fatalf("x=0 → 组装侧仍保最新轮全量：got %d %+v", len(z), z)
	}
}

// TestProjectThreeZones：三段齐备——完整区原文 / 简化区仅 text / 摘要区残留同按简化态投影；
// **预存 token 优先**（storedFull/storedBrief 尾部对齐覆盖实时估算）。
func TestProjectThreeZones(t *testing.T) {
	hist := []Message{
		{Role: "user", Kind: "text", Content: "q1"},
		{Role: "assistant", Content: "中间文本1", ToolCalls: []ToolCall{mkToolCall("c1", "read", `{}`)}},
		{Role: "tool", ToolCallID: "c1", Content: "结果1"},
		{Role: "user", Kind: "text", Content: "q2"},
		{Role: "assistant", Content: "结论2"},
		{Role: "user", Kind: "text", Content: "q3"},
		{Role: "assistant", Content: "结论3"},
	}
	storedFull := []int{10, 10, 10}
	storedBrief := []int{5, 5, 5}
	out, _ := Project(hist, 10, 15, 5, storedFull, storedBrief, false)
	if len(out) != 2+2+2 {
		t.Fatalf("三段拼接条数：got %d %+v", len(out), out)
	}
	if len(out[1].ToolCalls) != 0 || out[2].Content != "q2" || out[5].Content != "结论3" {
		t.Fatalf("三段内容不符：%+v", out)
	}
}

// TestProjectInjectedContinueSameTurn：恢复/续写注入的 user 消息（Kind=continue）
// 不计入新轮边界 → 仍属同一 turn；配合 forceFullLast 整轮全量。
func TestProjectInjectedContinueSameTurn(t *testing.T) {
	hist := []Message{
		{Role: "user", Kind: "text", Content: "问1"},
		{Role: "assistant", Content: "部分1"},
		{Role: "user", Kind: KindContinue, Content: ContinueText},
		{Role: "assistant", Content: "结论1"},
	}
	out, _ := Project(hist, 0, 0, 0, nil, nil, true)
	if len(out) != len(hist) {
		t.Fatalf("注入续写消息不应新开轮（应整轮全量）：got %d %+v", len(out), out)
	}
	if out[1].Content != "部分1" || out[2].Content != ContinueText {
		t.Fatalf("整轮全量应保留中间输出与注入消息：%+v", out)
	}
	bad := append([]Message{}, hist...)
	bad[2].Kind = "text"
	bo, _ := Project(bad, 1, 0, 0, nil, nil, true)
	if len(bo) != 2+2 || bo[0].Content != "问1" || bo[1].Content != "部分1" {
		t.Fatalf("无标记时应拆轮（原轮简化态）：got %d %+v", len(bo), bo)
	}
}

// TestApplyReasoningRule：DeepSeek 协议回传规则——带 tool_calls 的 assistant 保留 reasoning，
// 不带 tool_calls 的 assistant（纯文本轮）与 tool/user/system 一律清空；且不改动入参。
func TestApplyReasoningRule(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Kind: "text", Content: "q", Reasoning: "不应存在"},
		{Role: "assistant", Content: "中间", ToolCalls: []ToolCall{mkToolCall("c1", "read", "{}")}, Reasoning: "思考A"},
		{Role: "tool", ToolCallID: "c1", Content: "r", Reasoning: "不应存在"},
		{Role: "assistant", Content: "结论", Reasoning: "思考B"},
	}
	out := ApplyReasoningRule(msgs)
	if out[2].Reasoning != "思考A" {
		t.Fatalf("带 tool_calls 的 assistant 应回传 reasoning: %+v", out[2])
	}
	for i := range out {
		if i != 2 && out[i].Reasoning != "" {
			t.Fatalf("非工具调用消息不应回传 reasoning: out[%d]=%+v", i, out[i])
		}
	}
	if msgs[2].Reasoning != "思考A" || msgs[4].Reasoning != "思考B" {
		t.Fatalf("入参被篡改: %+v", msgs)
	}
}

// TestSplitTurnsAndBoundary：轮切分与 id 映射（尾部对齐 turn_tokens）。
func TestSplitTurnsAndBoundary(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Kind: "text", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Kind: KindContinue, Content: ContinueText},
		{Role: "assistant", Content: "a1b"},
		{Role: "user", Kind: "text", Content: "q2"},
	}
	if got := FirstTurnBoundary(msgs); got != 1 {
		t.Fatalf("首个轮边界应为 1，got %d", got)
	}
	turns, ids, histTurns := TurnGroups(msgs, msgs[:5], []TurnToken{{TurnID: "t1"}}, "t-cur")
	if len(turns) != 2 || histTurns != 1 {
		t.Fatalf("分组/历史轮数不符：turns=%d histTurns=%d", len(turns), histTurns)
	}
	if ids[0] != "t1" || ids[1] != "t-cur" {
		t.Fatalf("轮 id 映射不符：%v", ids)
	}
	z := ZoneIDsOf(ids, data.Zones{FullTurns: 1, BriefTurns: 1})
	if z.FullFrom != "t-cur" || z.BriefFrom != "t1" {
		t.Fatalf("三段边界换算不符：%+v", z)
	}
}
