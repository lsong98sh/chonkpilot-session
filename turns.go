// turn 切分、轮 id 映射与三段边界换算（纯函数；搬自 `src/lib/llm/server/assembly.go` +
// `assemble.go` 的 turn 边界部分，行为逐字不变）。
package session

import "github.com/chonkpilot/chonkpilot-session/store"

// 系统注入的用户消息 Kind（**不新开 turn 边界**——仍属当前 turn）：
//   - KindContinue：S6 断链 / length 自动续写注入的「继续」；
//   - KindResume：重启 / 死 turn 恢复续轮注入的 user 消息（恢复路径如注入用户消息用此标记）；
//   - KindNotify：工具完成通知。
const (
	KindNotify   = "notify"
	KindContinue = "continue"
	KindResume   = "resume"
)

// ContinueText 是自动续写注入的用户消息文本（配合 KindContinue）。
const ContinueText = "继续"

// IsTurnBoundary 判断一条消息是否为 turn 起点（用户提问）：role=user 且非系统注入消息
// （Kind=notify 工具通知 / continue 自动续写 / resume 恢复续轮）。
// 只按 Kind 标记判定、不按内容——用户真实提问（Kind=text）即便文本为"继续"也是新轮边界。
func IsTurnBoundary(m Message) bool {
	if m.Role != "user" {
		return false
	}
	switch m.Kind {
	case KindNotify, KindContinue, KindResume:
		return false
	}
	return true
}

// FirstTurnBoundary 返回首个轮边界下标（无用户提问 → -1）。
func FirstTurnBoundary(msgs []Message) int {
	for i, m := range msgs {
		if IsTurnBoundary(m) {
			return i
		}
	}
	return -1
}

// SplitTurns 按轮边界切分 turn 段（msgs[0] 必为轮边界，由调用方保证）。
func SplitTurns(msgs []Message) [][]Message {
	var turns [][]Message
	for _, m := range msgs {
		if IsTurnBoundary(m) {
			turns = append(turns, []Message{m})
			continue
		}
		turns[len(turns)-1] = append(turns[len(turns)-1], m)
	}
	return turns
}

// TurnGroups 把投影后的消息按轮边界分组，并把每轮映射到 turn id（尾部对齐 `turnTokens`）。
//
// 返回：
//   - turns = 各轮消息（升序；末轮通常是当前进行中的 turn）；
//   - ids = 与 turns 等长的 turn id（历史轮取 `turnTokens` 尾部；当前轮 = currentTurn；
//     无法映射 = ""）；
//   - histTurns = 前多少轮属于历史（不含当前进行中的 turn）。
//
// turn_tokens 的语义 = 会话各轮（**排除当前轮**、升序、可短可长），与历史轮按**尾部对齐**
// （同 `data.ResolveStoredTokens` 的口径）；历史分组数超出 turn_tokens 时，多出的（更早的）轮
// id 留空（不猜）。
func TurnGroups(projected, hist []Message, turnTokens []TurnToken, currentTurn string) ([][]Message, []string, int) {
	first := FirstTurnBoundary(projected)
	if first < 0 {
		return nil, nil, 0
	}
	turns := SplitTurns(projected[first:])
	histTurns := 0
	if hfirst := FirstTurnBoundary(hist); hfirst >= 0 {
		histTurns = len(SplitTurns(hist[hfirst:]))
		if histTurns > len(turns) {
			histTurns = len(turns)
		}
	}
	ids := make([]string, len(turns))
	off := len(turnTokens) - histTurns
	for i := 0; i < histTurns; i++ {
		if j := off + i; j >= 0 && j < len(turnTokens) {
			ids[i] = turnTokens[j].TurnID
		}
	}
	for i := histTurns; i < len(turns); i++ {
		ids[i] = currentTurn
	}
	return turns, ids, histTurns
}

// ZoneIDsOf 把三段**计数**（`LocateZones`，`Project` 原样带回）换算为 turn id 边界。
//
// 口径与投影一致：完整区 = 末尾 `FullTurns` 轮；简化区 = 再往前 `BriefTurns` 轮；其余 = 摘要区。
func ZoneIDsOf(ids []string, z store.Zones) ZoneIDs {
	if len(ids) == 0 {
		return ZoneIDs{}
	}
	fullStart := len(ids) - z.FullTurns
	if fullStart < 0 {
		fullStart = 0
	}
	briefStart := fullStart - z.BriefTurns
	if briefStart < 0 {
		briefStart = 0
	}
	out := ZoneIDs{}
	if fullStart < len(ids) {
		out.FullFrom = ids[fullStart]
	}
	if briefStart < fullStart {
		out.BriefFrom = ids[briefStart]
	}
	return out
}

// RetainStartIndex 返回**压缩保留段**的起点（分组下标）：= 简化区起点；无简化区 → 完整区起点；
// 保留段只覆盖历史轮（当前轮不写入快照）。无历史轮 → -1。
func RetainStartIndex(ids []string, z store.Zones, histTurns int) int {
	if histTurns <= 0 || len(ids) == 0 {
		return -1
	}
	start := len(ids) - z.FullTurns - z.BriefTurns
	if start < 0 {
		start = 0
	}
	if start >= histTurns {
		return -1
	}
	return start
}

// SplitTurnTokens 把 turn_tokens 拆成**升序**的完整态 / 简化态 token 两数组
// （供三段定位尾部对齐取用；缺值位留 0 = 缺值 → 调用方回退实时估算）。
func SplitTurnTokens(tokens []TurnToken) (full, brief []int) {
	if len(tokens) == 0 {
		return nil, nil
	}
	full = make([]int, len(tokens))
	brief = make([]int, len(tokens))
	for i, t := range tokens {
		if t.Full != nil {
			full[i] = *t.Full
		}
		if t.Brief != nil {
			brief[i] = *t.Brief
		}
	}
	return full, brief
}
