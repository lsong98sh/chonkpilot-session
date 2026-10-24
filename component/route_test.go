package component

import "testing"

// kindsOf 提取决策类型序列（便于断言）。
func kindsOf(ds []Decision) []DecisionKind {
	out := make([]DecisionKind, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Kind)
	}
	return out
}

// eqKinds 断言决策类型序列。
func eqKinds(t *testing.T, got []Decision, want ...DecisionKind) {
	t.Helper()
	g := kindsOf(got)
	if len(g) != len(want) {
		t.Fatalf("决策序列不符: got=%v want=%v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("决策序列不符: got=%v want=%v", g, want)
		}
	}
}

// TestRelay：单聊 / 子对话两条分支。
func TestRelay(t *testing.T) {
	ctx := Context{Self: "llm1", Session: SessionInfo{ID: "s1", Kind: "main"}}
	human := Utterance{From: Participant{Kind: "human", ID: "u1"}, Body: "你好"}
	self := Utterance{From: Participant{Kind: "llm", ID: "llm1"}, Body: "在"}

	r := Relay{}
	eqKinds(t, r.Dispatch(ctx, human), DecisionPersist, DecisionDeliverLocal)
	eqKinds(t, r.Dispatch(ctx, self), DecisionPersist, DecisionFinish)

	child := Relay{ReturnToParent: true}
	eqKinds(t, child.Dispatch(ctx, self), DecisionPersist, DecisionToParent)

	term := Relay{Terminator: TextMention{Marker: "TERMINATE"}}
	got := term.Dispatch(ctx, Utterance{From: Participant{Kind: "llm", ID: "llm1"}, Body: "done TERMINATE"})
	eqKinds(t, got, DecisionPersist, DecisionFinish)
	if got[1].Reason != "text_mention:TERMINATE" {
		t.Fatalf("收口原因不符: %+v", got[1])
	}
}

// TestFanout：扇出 → 收齐 → 下一轮 / 收口（2G §6.4 多 agent）。
func TestFanout(t *testing.T) {
	ctx := Context{
		Self:    "main-llm",
		Session: SessionInfo{ID: "main", Kind: "group_chat"},
		Round:   RoundState{Targets: []string{"A", "B"}},
	}
	f := Fanout{Terminator: TextMention{Marker: "DONE"}, Selector: FixedOrder{Order: []string{"A", "B"}}}

	// 主 llm 发言且未终止 → 扇出 A（Round=0）
	got := f.Dispatch(ctx, Utterance{From: Participant{Kind: "llm", ID: "main-llm"}, Body: "开工"})
	eqKinds(t, got, DecisionPersist, DecisionToChild)
	if got[1].Child != "A" {
		t.Fatalf("应收敛到 A: %+v", got[1])
	}

	// A 回流、B 未回 → 只 Persist
	got = f.Dispatch(ctx, Utterance{From: Participant{Kind: "agent", ID: "A"}, Body: "A 结论"})
	eqKinds(t, got, DecisionPersist)

	// 收齐（Returned=[A] + 本条 B）未终止 → 下一轮 B（Round=1）
	ctx.Round.Returned = []string{"A"}
	ctx.Round.Round = 1
	got = f.Dispatch(ctx, Utterance{From: Participant{Kind: "agent", ID: "B"}, Body: "B 结论"})
	eqKinds(t, got, DecisionPersist, DecisionToChild)
	if got[1].Child != "B" {
		t.Fatalf("下一轮应选 B: %+v", got[1])
	}

	// 收齐且终止 → Finish
	got = f.Dispatch(ctx, Utterance{From: Participant{Kind: "agent", ID: "B"}, Body: "全部 DONE"})
	eqKinds(t, got, DecisionPersist, DecisionFinish)

	// 主 llm 自己终止 → 直接 Finish
	got = f.Dispatch(ctx, Utterance{From: Participant{Kind: "llm", ID: "main-llm"}, Body: "DONE"})
	eqKinds(t, got, DecisionPersist, DecisionFinish)
}

// TestRecord：恒 [Persist]。
func TestRecord(t *testing.T) {
	eqKinds(t, Record{}.Dispatch(Context{}, Utterance{Body: "x"}), DecisionPersist)
}

// TestTerminators：TextMention / MaxRounds / AnyOf / AllOf。
func TestTerminators(t *testing.T) {
	ctx := Context{Round: RoundState{Round: 2}}
	if done, _ := (TextMention{Marker: "terminate"}).Done(ctx, Utterance{Body: "please Terminate now"}); !done {
		t.Fatal("TextMention 应大小写不敏感命中")
	}
	if done, _ := (TextMention{}).Done(ctx, Utterance{Body: "TERMINATE"}); done {
		t.Fatal("空 Marker 永不收口")
	}
	if done, _ := (MaxRounds{N: 3}).Done(ctx, Utterance{}); done {
		t.Fatal("Round=2 < N=3 不应收口")
	}
	if done, _ := (MaxRounds{N: 2}).Done(ctx, Utterance{}); !done {
		t.Fatal("Round=2 >= N=2 应收口")
	}
	any := AnyOf{MaxRounds{N: 9}, TextMention{Marker: "bye"}}
	if done, _ := any.Done(ctx, Utterance{Body: "bye"}); !done {
		t.Fatal("AnyOf 任一命中即收口")
	}
	all := AllOf{MaxRounds{N: 2}, TextMention{Marker: "bye"}}
	if done, _ := all.Done(ctx, Utterance{Body: "bye"}); !done {
		t.Fatal("AllOf 全命中应收口")
	}
	if done, _ := all.Done(ctx, Utterance{Body: "hi"}); done {
		t.Fatal("AllOf 未全命中不应收口")
	}
	if done, _ := (AllOf{}).Done(ctx, Utterance{}); done {
		t.Fatal("空 AllOf 永不收口")
	}
}

// TestSelectors：FixedOrder 轮转 / Manual 取 Targets。
func TestSelectors(t *testing.T) {
	fo := FixedOrder{Order: []string{"A", "B", "C"}}
	for round, want := range map[int]string{0: "A", 1: "B", 2: "C", 3: "A"} {
		got := fo.Next(Context{Round: RoundState{Round: round}})
		if len(got) != 1 || got[0] != want {
			t.Fatalf("FixedOrder round=%d got=%v want=%s", round, got, want)
		}
	}
	if got := (FixedOrder{}).Next(Context{}); len(got) != 0 {
		t.Fatalf("空 Order 应无目标: %v", got)
	}
	manual := Manual{}
	if got := manual.Next(Context{Round: RoundState{Targets: []string{"X", "Y"}}}); len(got) != 2 || got[0] != "X" {
		t.Fatalf("Manual 应返回 Targets: %v", got)
	}
	if got := manual.Next(Context{}); len(got) != 0 {
		t.Fatalf("无 Targets 应空: %v", got)
	}
}

// TestFanoutViaDispatch：策略接到组件上时，子会话目标只作为决策返回（不越权写别库）。
func TestFanoutViaDispatch(t *testing.T) {
	d := Fanout{Selector: FixedOrder{Order: []string{"child-A"}}}
	c, _, prjUsr := newCompD(t, d)
	dlg, err := c.CreateDialog(CreateDialogCmd{Title: "群聊", Kind: "group_chat"})
	if err != nil {
		t.Fatalf("CreateDialog: %v", err)
	}
	child, err := c.CreateSession(CreateSessionCmd{DialogID: dlg.DialogID, ParentID: dlg.SessionID, Kind: "sub_chat"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	res, err := c.Dispatch(DispatchCmd{
		DialogID: dlg.DialogID, SessionID: dlg.SessionID,
		Utterance: Utterance{From: Participant{Kind: "llm", ID: "main-llm"}, Body: "分发"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	eqKinds(t, res.Decisions, DecisionPersist, DecisionToChild)
	if res.Decisions[1].Child != "child-A" {
		t.Fatalf("ToChild 目标不符: %+v", res.Decisions[1])
	}
	db := openDlg(t, prjUsr, dlg.DialogID)
	if msgs := messagesOf(t, db, child.SessionID); len(msgs) != 0 {
		t.Fatalf("组件不得代投子会话: %d", len(msgs))
	}
}
