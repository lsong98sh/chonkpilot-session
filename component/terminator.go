// terminator.go —— 终止条件（2G §6.2）。
//
// 纯判断：输入 `Context`（外部派生的轮次状态）+ 本条 `Utterance`，输出「是否收口 + 原因」。
package component

import "strings"

// Terminator 判定大轮次是否收口。
type Terminator interface {
	Done(Context, Utterance) (bool, string)
}

// TextMention 命中关键词即收口（大小写不敏感；`Marker` 空 = 永不收口）。
type TextMention struct{ Marker string }

// Done 实现 Terminator。
func (t TextMention) Done(_ Context, u Utterance) (bool, string) {
	if strings.TrimSpace(t.Marker) == "" {
		return false, ""
	}
	if strings.Contains(strings.ToLower(u.Body), strings.ToLower(t.Marker)) {
		return true, "text_mention:" + t.Marker
	}
	return false, ""
}

// MaxRounds 轮次达到上限即收口（`N<=0` = 永不收口）。
type MaxRounds struct{ N int }

// Done 实现 Terminator。
func (t MaxRounds) Done(ctx Context, _ Utterance) (bool, string) {
	if t.N <= 0 || ctx.Round.Round < t.N {
		return false, ""
	}
	return true, "max_rounds"
}

// AnyOf 任一子条件收口即收口（空集 = 永不收口）。
type AnyOf []Terminator

// Done 实现 Terminator。
func (t AnyOf) Done(ctx Context, u Utterance) (bool, string) {
	for _, sub := range t {
		if sub == nil {
			continue
		}
		if done, reason := sub.Done(ctx, u); done {
			return true, reason
		}
	}
	return false, ""
}

// AllOf 全部子条件都收口才收口（空集 = 永不收口）。
type AllOf []Terminator

// Done 实现 Terminator。
func (t AllOf) Done(ctx Context, u Utterance) (bool, string) {
	if len(t) == 0 {
		return false, ""
	}
	reason := ""
	for _, sub := range t {
		if sub == nil {
			return false, ""
		}
		done, r := sub.Done(ctx, u)
		if !done {
			return false, ""
		}
		reason = r
	}
	return true, reason
}
