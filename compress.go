// 压缩的纯计算部分：任务构造、阈值判定、payload 构造、CAS 判定、快照消息构造，
// 以及一次完整压缩（摘要 → CAS 回填）的编排（搬自 `src/lib/llm/server/compress.go`，
// 行为逐字不变；副作用全部经 `SnapshotStore` / `Summarizer` 端口）。
//
// 语义（沿用既有定案，不新增配置键、不新增 MQ 主题）：
//  1. 输入 = 刚发出的 payload 复用：去掉**全部 system**、把**最后一条 user** 换成压缩要求；
//  2. 回填 = 会话快照 `[system 摘要] + 保留段`、`snapshot_turn = Anchor`；
//  3. CAS = 仅当快照仍停在压缩基准时才写，否则丢弃（不反查"该写到哪个 turn"，
//     同时天然保证 `snapshot_turn` 单调不减）。
package session

import "context"

// SummaryPrefix 是摘要消息前缀（与既有压缩快照同形，前端 / 历史识别口径不变）。
const SummaryPrefix = "[已压缩早前对话] "

// CompressionJob 是一次压缩的输入（全部来自组装结果，压缩期间不再读消息表）。
type CompressionJob struct {
	InstanceID string
	SessionID  string
	TurnID     string    // 当前进行中的 turn（仅日志）
	Anchor     string    // 新 snapshot_turn = Assembly.LastTurnID（CAS 期望值）
	Payload    []Message // 刚发出的 payload（含 system；压缩时去掉 system）
	Retain     []Message // 保留段（简化区 + 完整区的**历史轮**；当前轮不写入快照）
}

// NewJob 由组装结果构造压缩任务（Anchor 空 = 无已完成轮 → 调用方不应触发）。
func NewJob(r Result, instanceID, sessionID, turnID string) CompressionJob {
	return CompressionJob{
		InstanceID: instanceID,
		SessionID:  sessionID,
		TurnID:     turnID,
		Anchor:     r.LastTurnID,
		Payload:    r.Context.Messages,
		Retain:     r.RetainedMessages(),
	}
}

// Due 判定是否达压缩阈值（复用既有三段配置键，不新增键）：
// 历史 token > 完整区 token 上限 + 简化区 brief 预算 → 摘要区必然存在、压缩有收益。
// 两个键均 <= 0（= 两条件均不启用）= 不压缩（与 `data.LocateFullTurnCount` 口径一致）。
func Due(u TokenUsage, keepFullTokens, briefBudget int) bool {
	budget := 0
	if keepFullTokens > 0 {
		budget += keepFullTokens
	}
	if briefBudget > 0 {
		budget += briefBudget
	}
	if budget <= 0 {
		return false
	}
	return u.History > budget
}

// PromptMessages 构造压缩请求消息：**去掉全部 system**、把最后一条 user 换成压缩要求。
// 返回 ok=false = payload 里没有 user 消息 / 唯一一条就是 user（无历史可压缩）→ 无从压缩。
func PromptMessages(payload []Message, requirement string) ([]Message, bool) {
	out := make([]Message, 0, len(payload))
	lastUser := -1
	for _, m := range payload {
		if m.Role == "system" {
			continue
		}
		out = append(out, m)
		if m.Role == "user" {
			lastUser = len(out) - 1
		}
	}
	if lastUser < 0 || lastUser == 0 {
		return nil, false // 无 user / 唯一一条就是 user（无历史可压缩）
	}
	out = out[:lastUser]
	out = append(out, Message{Role: "user", Content: requirement})
	return out, true
}

// CASAccept 判定 CAS 是否命中：当前快照轮次必须仍等于压缩基准。
// 快照只会被终态写（新一轮）或压缩本身推进 → 不等即"已推进过"，本次结果必须丢弃
// （同时天然保证 `snapshot_turn` 单调不减：只在等于基准时写入同名值）。
func CASAccept(current, anchor string) bool {
	return anchor != "" && current == anchor
}

// SnapshotMessages 构造压缩后的快照消息：`[system 摘要] + 保留段`。
func SnapshotMessages(summary string, retain []Message) []Message {
	msgs := make([]Message, 0, len(retain)+1)
	msgs = append(msgs, Message{Role: "system", Content: SummaryPrefix + summary})
	msgs = append(msgs, retain...)
	return msgs
}

// CommitResult 是一次快照回填的结果（供宿主按既有文案记日志）。
type CommitResult struct {
	Written  bool     // 已写入快照
	CASMiss  bool     // CAS 基准不符 → 本次结果被丢弃
	Snapshot Snapshot // 写前读到的当前快照（CAS 判定值；无快照 = Found=false）
	Messages int      // 写入的消息条数（含摘要；Written 时有效）
}

// Commit 把摘要写回会话快照（**单事务**：摘要 + snapshot_turn 同一条记录），并做 CAS 判定。
//
// 无快照 → 丢弃（保守：不在无快照会话上凭空造快照）；CAS 不符 → 丢弃（不覆盖）。
func Commit(ctx context.Context, store SnapshotStore, job CompressionJob, summary string) (CommitResult, error) {
	cur, err := store.Current(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	res := CommitResult{Snapshot: cur}
	if !cur.Found {
		return res, nil
	}
	if !CASAccept(cur.Turn, job.Anchor) {
		res.CASMiss = true
		return res, nil
	}
	msgs := SnapshotMessages(summary, job.Retain)
	if err := store.Commit(ctx, Snapshot{Found: true, Turn: job.Anchor, Messages: msgs}); err != nil {
		return CommitResult{}, err
	}
	res.Written, res.Messages = true, len(msgs)
	return res, nil
}

// CompressOnce 一次完整压缩：构造 payload → 摘要（Summarizer 端口）→ CAS 回填快照。
// 返回 Written=true 表示已写入快照；false = 无需压缩 / CAS 丢弃（均非错误）。
func CompressOnce(ctx context.Context, store SnapshotStore, sum Summarizer, job CompressionJob, requirement string) (CommitResult, error) {
	if job.Anchor == "" {
		return CommitResult{}, nil
	}
	payload, ok := PromptMessages(job.Payload, requirement)
	if !ok {
		return CommitResult{}, nil
	}
	summary, err := sum.Summarize(ctx, payload)
	if err != nil {
		return CommitResult{}, err
	}
	return Commit(ctx, store, job, summary)
}
