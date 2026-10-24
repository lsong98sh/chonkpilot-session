// route.go —— 内置投递策略（2G §6.3）：Relay / Fanout / Record。
//
// 纯决策：不落库、不投递、不做 IO；`Persist` / `Finish` 由组件就地执行，
// 其余决策由外部串行邮箱执行。
package component

// Relay：单聊（`returnToParent=false`）与子对话（`true`）。
//
//   - `from != self`（人类 / 外部）→ `[Persist, DeliverLocal]`；
//   - `terminator.Done` 或 `from == self` → `returnToParent ? [Persist, ToParent] : [Persist, Finish]`。
type Relay struct {
	Terminator     Terminator
	ReturnToParent bool
}

// Dispatch 实现 Dispatcher。
func (r Relay) Dispatch(ctx Context, u Utterance) []Decision {
	if !isSelf(ctx, u) {
		return []Decision{{Kind: DecisionPersist}, {Kind: DecisionDeliverLocal, Reason: "relay"}}
	}
	_, reason := doneOf(r.Terminator, ctx, u)
	tail := Decision{Kind: DecisionFinish, Reason: firstNonEmpty(reason, "self")}
	if r.ReturnToParent {
		tail = Decision{Kind: DecisionToParent, Reason: firstNonEmpty(reason, "self")}
	}
	return []Decision{{Kind: DecisionPersist}, tail}
}

// Fanout：多 agent（主 session 扇出到子会话，子会话回流到主）。
//
//   - `from == self` 且未终止 → `[Persist, ToChild(selector.Next…)]`；
//   - `from == child` 未收齐 → `[Persist]`；
//   - 收齐且未终止 → `[Persist, ToChild(selector.Next…)]`；
//   - 收齐且终止（或 self 终止）→ `[Persist, Finish]`。
type Fanout struct {
	Terminator Terminator
	Selector   Selector
}

// Dispatch 实现 Dispatcher。
func (f Fanout) Dispatch(ctx Context, u Utterance) []Decision {
	done, reason := doneOf(f.Terminator, ctx, u)
	if isSelf(ctx, u) {
		if done {
			return []Decision{{Kind: DecisionPersist}, {Kind: DecisionFinish, Reason: reason}}
		}
		return toChildren(f.Selector, ctx, "fanout")
	}
	// from == child：按 targets 判定是否收齐（Returned ∪ {本条 from}）。
	returned := map[string]bool{}
	for _, id := range ctx.Round.Returned {
		returned[id] = true
	}
	if u.From.ID != "" {
		returned[u.From.ID] = true
	}
	all := len(ctx.Round.Targets) > 0
	for _, id := range ctx.Round.Targets {
		if !returned[id] {
			all = false
			break
		}
	}
	if !all {
		return []Decision{{Kind: DecisionPersist, Reason: "await-children"}}
	}
	if done {
		return []Decision{{Kind: DecisionPersist}, {Kind: DecisionFinish, Reason: reason}}
	}
	return toChildren(f.Selector, ctx, "fanout")
}

// Record：旁听 / 只入库（恒 `[Persist]`）。
type Record struct{}

// Dispatch 实现 Dispatcher。
func (Record) Dispatch(Context, Utterance) []Decision {
	return []Decision{{Kind: DecisionPersist}}
}

// doneOf 统一处理 nil Terminator（永不收口）。
func doneOf(t Terminator, ctx Context, u Utterance) (bool, string) {
	if t == nil {
		return false, ""
	}
	return t.Done(ctx, u)
}

// isSelf 判定发言者是否本会话的 llm（`Self` 未配置时，`kind=llm` 视为本会话）。
func isSelf(ctx Context, u Utterance) bool {
	if ctx.Self != "" {
		return u.From.ID == ctx.Self
	}
	return u.From.Kind == "llm"
}

// toChildren 按 Selector 产出 ToChild 决策（跳过本会话；无目标 → 只 Persist）。
func toChildren(sel Selector, ctx Context, reason string) []Decision {
	if sel == nil {
		return []Decision{{Kind: DecisionPersist}}
	}
	next := sel.Next(ctx)
	out := []Decision{{Kind: DecisionPersist}}
	for _, id := range next {
		if id == "" || id == ctx.Self {
			continue
		}
		out = append(out, Decision{Kind: DecisionToChild, Child: id, Reason: reason})
	}
	return out
}
