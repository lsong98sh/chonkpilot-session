// selector.go —— 下一轮目标选择（2G §6.2）。
package component

// Selector 选出下一轮要投递的子会话（空 = 无目标）。
type Selector interface {
	Next(Context) []string
}

// FixedOrder 按固定顺序轮转（= round_robin；以 `Context.Round.Round` 为偏移）。
type FixedOrder struct{ Order []string }

// Next 实现 Selector。
func (s FixedOrder) Next(ctx Context) []string {
	n := len(s.Order)
	if n == 0 {
		return nil
	}
	start := ctx.Round.Round % n
	if start < 0 {
		start = 0
	}
	return []string{s.Order[start]}
}

// Manual 用 `Context.Round.Targets`（外部显式指定；空 = 无目标）。
type Manual struct{}

// Next 实现 Selector。
func (Manual) Next(ctx Context) []string {
	if len(ctx.Round.Targets) == 0 {
		return nil
	}
	out := make([]string, len(ctx.Round.Targets))
	copy(out, ctx.Round.Targets)
	return out
}
