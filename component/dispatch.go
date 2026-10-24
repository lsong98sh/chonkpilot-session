// dispatch.go —— 内容唯一入口 Dispatch + 投递决策类型（SESSION-COMP-1 P1b，2G §4.2 / §6）。
//
// 边界：组件**只执行本地动作**（Persist 落库、Finish 关轮），投递（DeliverLocal / ToChild /
// ToParent）**只产出决策**，由外部串行邮箱执行（组件无 IO）。
//
// P1b 只内置 `PersistOnly`（恒 [Persist]）；`Relay` / `Fanout` / `Record` 与 Terminator /
// Selector 属于 P2（2G §6.2/§6.3）。
package component

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/chonkpilot/chonkpilot-data"
)

// ParticipantID 是参与者标识（2G §6.1 `Context.Self`）。
type ParticipantID = string

// DecisionKind 是投递决策类型。
type DecisionKind string

const (
	DecisionPersist      DecisionKind = "Persist"
	DecisionDeliverLocal DecisionKind = "DeliverLocal"
	DecisionToChild      DecisionKind = "ToChild"
	DecisionToParent     DecisionKind = "ToParent"
	DecisionFinish       DecisionKind = "Finish"
)

// Decision 是一条决策（Persist / DeliverLocal / ToChild / ToParent / Finish）。
type Decision struct {
	Kind   DecisionKind
	Child  string // ToChild 的目标 session（同一 dialog 内）
	Reason string
}

// SessionInfo 是决策上下文里的会话信息（id / parent / kind / participants）。
type SessionInfo struct {
	ID           string
	ParentID     string
	Kind         string
	Participants []Participant
}

// RoundState 是大轮次状态（外部从 turns/messages 派生后注入；组件无内存态）。
type RoundState struct {
	Round    int
	Returned []string // 已回流的 agent
	Targets  []string // 本轮目标序列
}

// DispatcherConf 是策略参数（外部注入；组件不解释语义）。
type DispatcherConf struct {
	EndMarker      string
	RoundLimit     int
	ReturnToParent bool
	Targets        []string
}

// Context 是 Dispatcher 的输入（纯数据，无 IO）。
type Context struct {
	DialogID string
	Session  SessionInfo
	Self     ParticipantID
	Round    RoundState
	Conf     DispatcherConf
}

// Dispatcher 是投递策略接口（纯决策；可被外部替换）。
type Dispatcher interface {
	Dispatch(Context, Utterance) []Decision
}

// PersistOnly 是 P1b 的缺省策略：恒 [Persist]（只入库；P2 提供 Relay / Fanout / Record）。
type PersistOnly struct{}

// Dispatch 实现 Dispatcher。
func (PersistOnly) Dispatch(Context, Utterance) []Decision {
	return []Decision{{Kind: DecisionPersist}}
}

// Utterance 是一条话语（2G §2）。
type Utterance struct {
	From        Participant
	Role        string // "" → 由 From.Kind 推导（human→user；其余→assistant）
	Body        string
	At          string // "" → 当前时刻（UTC 纳秒固定 9 位）
	ClientMsgID string // 幂等键（同 session 内唯一；空 = 不参与幂等）
}

// DispatchCmd 是内容唯一入口的入参。
type DispatchCmd struct {
	DialogID  string
	SessionID string
	Utterance
}

// DispatchRes 是结果（Replayed=true 表示幂等命中：不写、不发、不投递）。
type DispatchRes struct {
	MessageID string
	TurnID    string
	Decisions []Decision
	Replayed  bool
}

// Dispatch 落一条话语并产出投递决策。
//
// 执行序（2G §4.2）：幂等检查 → 自动开轮 → 调 Dispatcher（纯）→ 同事务落库（message +
// turn/session updated_at）→ 提交后发事件 → 返回决策。
func (c *Component) Dispatch(cmd DispatchCmd) (DispatchRes, error) {
	if cmd.DialogID == "" {
		return DispatchRes{}, Err(CodeDialogRequired, "dialog_id required")
	}
	if cmd.SessionID == "" {
		return DispatchRes{}, Err(CodeSessionNotFound, "session_id required")
	}
	if cmd.Body == "" {
		return DispatchRes{}, Err(CodeInvalid, "body required")
	}
	role := cmd.Role
	if role == "" {
		if cmd.From.Kind == "human" {
			role = "user"
		} else {
			role = "assistant"
		}
	}

	sdb, release, err := c.openDialog(cmd.DialogID)
	if err != nil {
		return DispatchRes{}, c.mapOpenErr(err, cmd.DialogID)
	}
	sessRec, ok, err := sessionByID(sdb, cmd.SessionID)
	if err != nil {
		release()
		return DispatchRes{}, Err(CodeBackend, "读 sessions: %v", err)
	}
	if !ok {
		release()
		return DispatchRes{}, Err(CodeSessionNotFound, "session %s 不在对话 %s", cmd.SessionID, cmd.DialogID)
	}

	// ① 幂等：client_msg_id 命中 → 回放（不写、不发、不投递）。
	if cmd.ClientMsgID != "" {
		hit, err := messagesByClientMsgID(sdb, cmd.SessionID, cmd.ClientMsgID)
		if err != nil {
			release()
			return DispatchRes{}, Err(CodeBackend, "查幂等键: %v", err)
		}
		if hit != nil {
			release()
			return DispatchRes{
				MessageID: sval(hit[data.KeyField]),
				TurnID:    sval(hit["turn_id"]),
				Decisions: nil,
				Replayed:  true,
			}, nil
		}
	}

	// ② 自动开轮（无 running turn 才新建）。
	turnRec, hasTurn, err := findOpenTurn(sdb, cmd.SessionID)
	if err != nil {
		release()
		return DispatchRes{}, Err(CodeBackend, "读 turns: %v", err)
	}
	turnID := sval(turnRec["turn_id"])
	opened := false
	if !hasTurn {
		turnID = mustID()
		opened = true
	}

	// ③ 纯决策（不持锁、不做 IO）。
	dispatcher := c.dispatcher
	if dispatcher == nil {
		dispatcher = PersistOnly{}
	}
	decisions := dispatcher.Dispatch(Context{
		DialogID: cmd.DialogID,
		Session: SessionInfo{
			ID:           cmd.SessionID,
			ParentID:     sval(sessRec["parent_id"]),
			Kind:         sval(sessRec["kind"]),
			Participants: participantsOf(sessRec),
		},
		Self: cmd.From.ID,
	}, cmd.Utterance)
	// 硬约束：human 说话必须落库（dispatcher 漏给也强制补 Persist）。
	if cmd.From.Kind == "human" && !hasDecision(decisions, DecisionPersist) {
		decisions = append([]Decision{{Kind: DecisionPersist, Reason: "human-force-persist"}}, decisions...)
	}

	// ④ 同一 bbolt 事务：消息行 + turn/session updated_at（+ 自动开轮时建 turn 行）。
	now := cmd.At
	if now == "" {
		now = c.nowStr()
	}
	msgKey := newMessageKey(time.Now())
	msgRec := data.Record{
		"session_id": cmd.SessionID,
		"turn_id":    turnID,
		"role":       role,
		"content":    cmd.Body,
		"created_at": now,
	}
	if cmd.ClientMsgID != "" {
		msgRec["client_msg_id"] = cmd.ClientMsgID
	}
	if cmd.From.ID != "" && cmd.From.Kind != "human" && cmd.From.Kind != "llm" {
		msgRec["sender_agent"] = cmd.From.ID // 群聊发言者（agent）；人类 / 单 llm 不写键
	}
	if err := sdb.Update(func(tx *data.Tx) error {
		if opened {
			if err := tx.Put("turns", turnID, data.Record{
				"turn_id": turnID, "session_id": cmd.SessionID, "status": "running",
				"created_at": now, "updated_at": now,
			}); err != nil {
				return err
			}
		} else if err := touchTurn(tx, turnID, now); err != nil {
			return err
		}
		if err := tx.Insert("messages", msgKey, msgRec); err != nil {
			return err
		}
		return touchSession(tx, cmd.SessionID, now)
	}); err != nil {
		release()
		return DispatchRes{}, Err(CodeBackend, "落库: %v", err)
	}
	release()

	// ⑤ 提交后发事件（不持锁；允许回调内再 Dispatch）。
	if opened {
		c.emit(Event{Kind: EventTurnOpened, DialogID: cmd.DialogID, SessionID: cmd.SessionID, TurnID: turnID})
	}
	c.emit(Event{
		Kind: EventMessageAppended, DialogID: cmd.DialogID, SessionID: cmd.SessionID,
		TurnID: turnID, MessageID: msgKey, Role: role,
	})

	// ⑥ Finish 决策就地关轮（本地动作），投递类决策原样返回由外部执行。
	for _, d := range decisions {
		if d.Kind == DecisionFinish {
			if err := c.closeTurn(cmd.DialogID, cmd.SessionID, turnID, "completed", firstNonEmpty(d.Reason, "finished")); err != nil {
				return DispatchRes{MessageID: msgKey, TurnID: turnID, Decisions: decisions},
					Err(CodeBackend, "关轮: %v", err)
			}
			break
		}
	}
	return DispatchRes{MessageID: msgKey, TurnID: turnID, Decisions: decisions}, nil
}

// hasDecision 判定决策集里是否含某类。
func hasDecision(ds []Decision, kind DecisionKind) bool {
	for _, d := range ds {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

// participantsOf 读 sessions 行的 participants（缺失 → nil）。
func participantsOf(rec data.Record) []Participant {
	raw, ok := rec["participants"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []Participant:
		return v
	case []any:
		out := make([]Participant, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, Participant{
					Kind: sval(m["kind"]), ID: sval(m["id"]), LLM: sval(m["llm"]),
				})
			}
		}
		return out
	}
	return nil
}

// messagesByClientMsgID 按 (session_id, client_msg_id) 命中一条消息（P0 索引）。
func messagesByClientMsgID(sdb *data.DB, sessionID, clientMsgID string) (data.Record, error) {
	recs, _, err := sdb.Table("messages").Query(data.Query{
		Where: data.Record{"session_id": sessionID, "client_msg_id": clientMsgID}, Limit: 1,
	})
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return recs[0], nil
}

// newMessageKey 生成消息主键（时间有序）：M + UTC 时间戳(14) + 微秒(6) + 4 位随机。
func newMessageKey(t time.Time) string {
	t = t.UTC()
	var b [2]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("M%s%06d%s", t.Format("20060102150405"), t.Nanosecond()/1000, hex.EncodeToString(b[:]))
}
