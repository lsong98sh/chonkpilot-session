// events.go —— 组件事件（2G §4.3）。Sink 同步回调：只入队、不 panic、不长阻塞。
//
// P1a 只发目录类事件（DialogCreated / DialogDeleted / SessionCreated / TitleChanged）；
// 内容类（MessageAppended / TurnOpened / TurnClosed / SnapshotSet / Finished）随 P1b 接入。
package component

// EventKind 是事件类型。
type EventKind string

const (
	EventDialogCreated   EventKind = "DialogCreated"
	EventDialogDeleted   EventKind = "DialogDeleted"
	EventSessionCreated  EventKind = "SessionCreated"
	EventTitleChanged    EventKind = "TitleChanged"
	EventMessageAppended EventKind = "MessageAppended"
	EventTurnOpened      EventKind = "TurnOpened"
	EventTurnClosed      EventKind = "TurnClosed"
	EventSnapshotSet     EventKind = "SnapshotSet"
	EventFinished        EventKind = "Finished"
)

// Event 是一个组件事件（字段按事件类型取舍；空字段省略）。
type Event struct {
	Kind EventKind
	// 寻址
	DialogID  string
	SessionID string
	ParentID  string
	// 目录 / 会话
	Title       string
	SessionKind string // sessions.kind：main / sub_chat / group_chat / work_session / sub_session
	// TitleChanged：true = 改的是对话标题（主 session），false = 子会话
	IsDialogTitle bool
	// 内容类（P1b）
	TurnID       string
	MessageID    string
	Role         string
	Status       string
	FinishReason string
	SnapshotTurn string
	Reason       string
}

// Sink 是事件出口（实现方负责排队；不得阻塞 / panic）。
type Sink interface{ Emit(Event) }

// SinkFunc 便于测试 / 简单宿主实现。
type SinkFunc func(Event)

// Emit 实现 Sink。
func (f SinkFunc) Emit(ev Event) {
	if f != nil {
		f(ev)
	}
}

// Participant 是会话参与者（`sessions.participants`）。
//
// 校验（2G §11 用例 3）：同一 session 内 `kind=llm`（或非空 `LLM`）最多 1 个；
// 超限 → `E_PARTICIPANT_LIMIT`。
type Participant struct {
	Kind string `json:"kind"` // human / llm / agent
	ID   string `json:"id"`
	LLM  string `json:"llm,omitempty"`
}

// validateParticipants 校验参与者上限。
func validateParticipants(ps []Participant) error {
	n := 0
	for _, p := range ps {
		if p.Kind == "llm" || p.LLM != "" {
			n++
		}
	}
	if n > 1 {
		return Err(CodeParticipantLimit, "同一 session 最多 1 个 llm 参与者（实际 %d）", n)
	}
	return nil
}
