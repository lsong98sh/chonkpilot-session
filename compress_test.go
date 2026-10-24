// 压缩纯计算部分的 L1 直测（期望值搬自 `src/lib/llm/server/compress_test.go`）：
// 输入构造（去 system / 换末条 user）、阈值判定、CAS 命中与丢弃、快照消息构造。
package session

import (
	"context"
	"errors"
	"testing"
)

// TestPromptMessages：压缩输入 = 组装 payload **去掉全部 system**、
// 把最后一条 user 换成压缩要求（其后消息不再携带）。
func TestPromptMessages(t *testing.T) {
	payload := []Message{
		{Role: "system", Content: "系统提示词"},
		{Role: "system", Content: "记忆指引"},
		{Role: "system", Content: "资产指引"},
		{Role: "user", Kind: "text", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Kind: "text", Content: "q2"},
	}
	out, ok := PromptMessages(payload, "SUMMARY-REQ")
	if !ok {
		t.Fatal("应构造成功")
	}
	if len(out) != 3 {
		t.Fatalf("应为 [q1, a1, 要求] 三条，got %+v", out)
	}
	for _, m := range out {
		if m.Role == "system" {
			t.Fatalf("压缩输入不得含 system: %+v", out)
		}
	}
	if out[0].Content != "q1" || out[1].Content != "a1" {
		t.Fatalf("历史消息应原样保留: %+v", out)
	}
	if out[2].Role != "user" || out[2].Content != "SUMMARY-REQ" {
		t.Fatalf("末条 user 应换成压缩要求: %+v", out[2])
	}
	// 无 user / 仅一条 user（无历史）→ 不构造。
	if _, ok := PromptMessages([]Message{{Role: "system", Content: "s"}}, "R"); ok {
		t.Fatal("无 user 不应构造")
	}
	if _, ok := PromptMessages([]Message{{Role: "user", Content: "q"}}, "R"); ok {
		t.Fatal("仅一条 user（无历史）不应构造")
	}
}

// TestCASAccept：只有当前快照仍停在压缩基准时才允许写入。
func TestCASAccept(t *testing.T) {
	if !CASAccept("t3", "t3") {
		t.Fatal("快照轮次 == 基准 → 命中")
	}
	if CASAccept("t4", "t3") {
		t.Fatal("快照已推进 → 丢弃")
	}
	if CASAccept("t3", "") {
		t.Fatal("无基准 → 不写")
	}
}

// TestDue：阈值 = keep_full_max_tokens + compress_token_threshold（不新增配置键）。
func TestDue(t *testing.T) {
	cases := []struct {
		name     string
		history  int
		keepFull int
		brief    int
		want     bool
	}{
		{"未达阈值", 100, 24000, 20000, false},
		{"超过两段预算", 50000, 24000, 20000, true},
		{"仅 token 条件", 30000, 0, 20000, true},
		{"两条件均不启用 → 不压缩", 999999, 0, 0, false},
		{"负数按不启用", 999999, -1, -1, false},
	}
	for _, c := range cases {
		if got := Due(TokenUsage{History: c.history}, c.keepFull, c.brief); got != c.want {
			t.Fatalf("%s: Due(history=%d, keepFull=%d, brief=%d) = %v, want %v",
				c.name, c.history, c.keepFull, c.brief, got, c.want)
		}
	}
}

// TestSnapshotMessages：`[system 摘要] + 保留段`（前缀与既有快照同形）。
func TestSnapshotMessages(t *testing.T) {
	out := SnapshotMessages("摘要正文", []Message{{Role: "user", Kind: "text", Content: "q1"}})
	if len(out) != 2 {
		t.Fatalf("应为摘要 + 保留段两条，got %+v", out)
	}
	if out[0].Role != "system" || out[0].Content != SummaryPrefix+"摘要正文" {
		t.Fatalf("首条应为摘要: %+v", out[0])
	}
	if out[1].Content != "q1" {
		t.Fatalf("保留段应原样保留: %+v", out[1])
	}
}

// ── 端口假实现（压缩编排）────────────────────────────────

type fakeStore struct {
	cur      Snapshot
	curErr   error
	setErr   error
	commits  []Snapshot
	commitNo int
}

func (f *fakeStore) Current(context.Context) (Snapshot, error) { return f.cur, f.curErr }

func (f *fakeStore) Commit(_ context.Context, snap Snapshot) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.commitNo++
	f.commits = append(f.commits, snap)
	return nil
}

type fakeSummarizer struct {
	text  string
	err   error
	calls int
	got   []Message
}

func (f *fakeSummarizer) Summarize(_ context.Context, msgs []Message) (string, error) {
	f.calls++
	f.got = msgs
	return f.text, f.err
}

// TestCompressOnceCommit：一次完整压缩——payload 去 system、摘要由端口产出、
// 快照写入 `[摘要] + 保留段` 且 snapshot_turn = 锚点。
func TestCompressOnceCommit(t *testing.T) {
	store := &fakeStore{cur: Snapshot{Found: true, Turn: "t1"}}
	sum := &fakeSummarizer{text: "摘要正文"}
	job := CompressionJob{
		SessionID: "s", Anchor: "t1",
		Payload: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Kind: "text", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Kind: "text", Content: "q2"},
		},
		Retain: []Message{{Role: "user", Kind: "text", Content: "q1"}, {Role: "assistant", Content: "a1"}},
	}
	res, err := CompressOnce(context.Background(), store, sum, job, "REQ")
	if err != nil {
		t.Fatalf("CompressOnce: %v", err)
	}
	if !res.Written || res.Messages != 3 {
		t.Fatalf("应写入摘要 + 保留段（3 条）: %+v", res)
	}
	if sum.calls != 1 || len(sum.got) != 3 || sum.got[2].Content != "REQ" {
		t.Fatalf("摘要输入应为去 system + 末条 user 换要求: %+v", sum.got)
	}
	if len(store.commits) != 1 || store.commits[0].Turn != "t1" || len(store.commits[0].Messages) != 3 {
		t.Fatalf("快照写入不符: %+v", store.commits)
	}
}

// TestCompressOnceDrops：CAS 基准不符 / 无快照 / 无历史 user → 不写、不报错。
func TestCompressOnceDrops(t *testing.T) {
	sum := &fakeSummarizer{text: "x"}
	job := CompressionJob{
		Anchor: "t1",
		Payload: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "q2"},
		},
	}
	// ① CAS 不符
	store := &fakeStore{cur: Snapshot{Found: true, Turn: "t9"}}
	res, err := CompressOnce(context.Background(), store, sum, job, "REQ")
	if err != nil || res.Written || !res.CASMiss || res.Snapshot.Turn != "t9" {
		t.Fatalf("CAS 不符应丢弃: res=%+v err=%v", res, err)
	}
	if len(store.commits) != 0 {
		t.Fatal("CAS 不符不应写入")
	}
	// ② 无快照
	empty := &fakeStore{}
	res, err = CompressOnce(context.Background(), empty, sum, job, "REQ")
	if err != nil || res.Written || res.CASMiss {
		t.Fatalf("无快照应丢弃（非 CAS 错误）: res=%+v err=%v", res, err)
	}
	// ③ 无锚点
	res, err = CompressOnce(context.Background(), store, sum, CompressionJob{}, "REQ")
	if err != nil || res.Written {
		t.Fatalf("无锚点应直接返回: res=%+v err=%v", res, err)
	}
	// ④ 摘要失败 → 原样出错（不写快照）
	store2 := &fakeStore{cur: Snapshot{Found: true, Turn: "t1"}}
	boom := errors.New("llm down")
	_, err = CompressOnce(context.Background(), store2, &fakeSummarizer{err: boom}, job, "REQ")
	if !errors.Is(err, boom) {
		t.Fatalf("摘要失败应原样返回: %v", err)
	}
	if len(store2.commits) != 0 {
		t.Fatal("摘要失败不应写入")
	}
}
