// dialog.go —— 对话 / 会话树 API（SESSION-COMP-1 P1a，2G §4）。
//
// 存储语义：
//   - 一库 = 一棵会话树；恒有且仅有 1 个主 session（`parent_id == ""`）；
//   - `dialogs` 行 = 可重建目录（title/kind/updated_at 是主 session 的副本，尽力双写 + 对账收敛）；
//   - 写序：**先库、后目录行**（崩在中间 → 文件/会话在、目录行缺 → 启动对账补行）。
package component

import (
	"sort"
	"strings"
	"unicode"

	"github.com/chonkpilot/chonkpilot-data"
)

// ── 命令 / 结果 ─────────────────────────────────────────────────

// CreateDialogCmd 新建对话（`dialog_id` 与主 `session_id` 均由组件生成）。
type CreateDialogCmd struct {
	Title        string
	Kind         string // "" → main
	Participants []Participant
}

// CreateDialogRes 是新建结果。
type CreateDialogRes struct {
	DialogID  string `json:"dialog_id"`
	SessionID string `json:"session_id"`
}

// EnsureDialogCmd 幂等建对话（`DialogID` 必填 = 幂等键）。
type EnsureDialogCmd struct {
	DialogID      string
	Title         string
	Kind          string
	Participants  []Participant
	MainSessionID string // "" = 组件生成（仅当库内还没有主 session 时使用）
}

// EnsureDialogRes 是幂等结果。
type EnsureDialogRes struct {
	DialogID  string `json:"dialog_id"`
	SessionID string `json:"session_id"`
	Created   bool   `json:"created"` // false = 已存在（未新建、未发事件）
}

// RenameDialogCmd 改主 session 标题（同时同步 dialogs.title）。
type RenameDialogCmd struct {
	DialogID string
	Title    string
}

// RenameSessionCmd 改子会话标题（主 session 会同步 dialogs.title）。
type RenameSessionCmd struct {
	DialogID  string
	SessionID string
	Title     string
}

// DeleteDialogCmd 删对话（目录行 + 库文件，幂等）。
type DeleteDialogCmd struct{ DialogID string }

// ListDialogsCmd 列对话（只读 prjusr，不打开任何对话库）。
type ListDialogsCmd struct{}

// CreateSessionCmd 建子会话（`ParentID` 必填 = 只能建子会话）。
type CreateSessionCmd struct {
	DialogID     string
	SessionID    string // "" = 组件生成
	ParentID     string
	Kind         string // "" → sub_chat
	Title        string
	Participants []Participant
}

// CreateSessionRes 是建子会话结果。
type CreateSessionRes struct {
	DialogID  string `json:"dialog_id"`
	SessionID string `json:"session_id"`
}

// ListSessionsCmd 列会话树（短开对话库）。
type ListSessionsCmd struct{ DialogID string }

// ListSessionsRes 是会话树结果（按 created_at 升序）。
type ListSessionsRes struct {
	DialogID string       `json:"dialog_id"`
	Sessions []SessionRow `json:"sessions"`
}

// ── API ────────────────────────────────────────────────────────

// CreateDialog 新建对话（生成 dialog_id + 主 session_id；发 DialogCreated）。
//
// 写序：先建库并写主 session 行，再写 `dialogs` 行（跨库非原子 → 由对账收敛）。
func (c *Component) CreateDialog(cmd CreateDialogCmd) (CreateDialogRes, error) {
	if err := validateParticipants(cmd.Participants); err != nil {
		return CreateDialogRes{}, err
	}
	dialogID, err := data.NewDialogID()
	if err != nil {
		return CreateDialogRes{}, Err(CodeBackend, "生成 dialog_id: %v", err)
	}
	sessionID, err := data.NewDialogID() // 同一 UUID v4 生成器（session 与 dialog 无关联）
	if err != nil {
		return CreateDialogRes{}, Err(CodeBackend, "生成 session_id: %v", err)
	}
	kind := firstNonEmpty(cmd.Kind, "main")
	title := firstNonEmpty(cmd.Title, sessionID)

	if err := c.writeSession(dialogID, sessionID, title, kind, "", cmd.Participants); err != nil {
		return CreateDialogRes{}, err
	}
	cat, release, err := c.catalog()
	if err != nil {
		return CreateDialogRes{}, Err(CodeBackend, "打开 prjusr: %v", err)
	}
	defer release()
	if err := putDialogRow(cat, DialogRow{DialogID: dialogID, Title: title, Kind: kind, MainSessionID: sessionID, UpdatedAt: c.nowStr()}); err != nil {
		return CreateDialogRes{}, Err(CodeBackend, "写 dialogs 行: %v", err)
	}
	c.emit(Event{Kind: EventDialogCreated, DialogID: dialogID, SessionID: sessionID, Title: title, SessionKind: kind})
	return CreateDialogRes{DialogID: dialogID, SessionID: sessionID}, nil
}

// EnsureDialog 幂等建对话：已存在 → 原样返回（Created=false，不发事件）。
//
// 半创建态（目录行在、库空 / 无主 session）→ 补齐主 session 行并按「已存在」返回（Created=false，
// 与 2G §8「半创建会话视为空对话」一致；是否补发事件以「是否新建主 session」为准）。
func (c *Component) EnsureDialog(cmd EnsureDialogCmd) (EnsureDialogRes, error) {
	if strings.TrimSpace(cmd.DialogID) == "" {
		return EnsureDialogRes{}, Err(CodeDialogRequired, "dialog_id required")
	}
	if err := validateParticipants(cmd.Participants); err != nil {
		return EnsureDialogRes{}, err
	}
	cat, release, err := c.catalog()
	if err != nil {
		return EnsureDialogRes{}, Err(CodeBackend, "打开 prjusr: %v", err)
	}
	_, exists, err := getDialogRow(cat, cmd.DialogID)
	release()
	if err != nil {
		return EnsureDialogRes{}, Err(CodeBackend, "读 dialogs 行: %v", err)
	}

	sdb, relDB, err := c.openDialog(cmd.DialogID)
	if err != nil {
		return EnsureDialogRes{}, Err(CodeBackend, "打开对话库: %v", err)
	}
	main, hasMain, err := mainSession(sdb)
	relDB()
	if err != nil {
		return EnsureDialogRes{}, Err(CodeBackend, "读 sessions: %v", err)
	}
	if hasMain {
		sid := sval(main["session_id"])
		return EnsureDialogRes{DialogID: cmd.DialogID, SessionID: sid}, nil
	}

	// 库空 / 无主 session → 补主 session（幂等键 = DialogID 已存在）。
	sessionID := cmd.MainSessionID
	if sessionID == "" {
		sessionID = mustID()
	}
	kind := firstNonEmpty(cmd.Kind, "main")
	title := firstNonEmpty(cmd.Title, sessionID)
	if err := c.writeSession(cmd.DialogID, sessionID, title, kind, "", cmd.Participants); err != nil {
		return EnsureDialogRes{}, err
	}
	cat2, release2, err := c.catalog()
	if err != nil {
		return EnsureDialogRes{}, Err(CodeBackend, "打开 prjusr: %v", err)
	}
	defer release2()
	if err := putDialogRow(cat2, DialogRow{DialogID: cmd.DialogID, Title: title, Kind: kind, MainSessionID: sessionID, UpdatedAt: c.nowStr()}); err != nil {
		return EnsureDialogRes{}, Err(CodeBackend, "写 dialogs 行: %v", err)
	}
	if !exists { // 只有首次落库才发事件（半创建补齐不算「新建对话」）
		c.emit(Event{Kind: EventDialogCreated, DialogID: cmd.DialogID, SessionID: sessionID, Title: title, SessionKind: kind})
	}
	return EnsureDialogRes{DialogID: cmd.DialogID, SessionID: sessionID, Created: !exists}, nil
}

// RenameDialog 改主 session 标题（同步 dialogs.title；幂等，值不变也写一次 updated_at）。
func (c *Component) RenameDialog(cmd RenameDialogCmd) error {
	if cmd.DialogID == "" {
		return Err(CodeDialogRequired, "dialog_id required")
	}
	if err := validateTitle(cmd.Title); err != nil {
		return err
	}
	sdb, release, err := c.openDialog(cmd.DialogID)
	if err != nil {
		return c.mapOpenErr(err, cmd.DialogID)
	}
	main, hasMain, err := mainSession(sdb)
	if err != nil {
		release()
		return Err(CodeBackend, "读 sessions: %v", err)
	}
	if !hasMain {
		release()
		return Err(CodeSessionNotFound, "对话 %s 无主 session", cmd.DialogID)
	}
	sid := sval(main["session_id"])
	now := c.nowStr()
	if err := sdb.Table("sessions").UpdateIn(sid, func(rec data.Record) data.Record {
		if len(rec) == 0 {
			return nil
		}
		rec["title"] = cmd.Title
		rec["updated_at"] = now
		return rec
	}); err != nil {
		release()
		return Err(CodeBackend, "改会话标题: %v", err)
	}
	release()

	cat, relCat, err := c.catalog()
	if err != nil {
		return Err(CodeBackend, "打开 prjusr: %v", err)
	}
	defer relCat()
	if err := cat.Table("dialogs").UpdateIn(cmd.DialogID, func(rec data.Record) data.Record {
		if len(rec) == 0 {
			return nil
		}
		rec["title"] = cmd.Title
		rec["updated_at"] = now
		return rec
	}); err != nil {
		return Err(CodeBackend, "同步 dialogs.title: %v", err)
	}
	c.emit(Event{Kind: EventTitleChanged, DialogID: cmd.DialogID, SessionID: sid, Title: cmd.Title, IsDialogTitle: true})
	return nil
}

// RenameSession 改子会话标题；若改的是主 session，同步 dialogs.title（尽力双写）。
func (c *Component) RenameSession(cmd RenameSessionCmd) error {
	if cmd.DialogID == "" {
		return Err(CodeDialogRequired, "dialog_id required")
	}
	if cmd.SessionID == "" {
		return Err(CodeSessionNotFound, "session_id required")
	}
	if err := validateTitle(cmd.Title); err != nil {
		return err
	}
	sdb, release, err := c.openDialog(cmd.DialogID)
	if err != nil {
		return c.mapOpenErr(err, cmd.DialogID)
	}
	rec, ok, err := sessionByID(sdb, cmd.SessionID)
	if err != nil {
		release()
		return Err(CodeBackend, "读 sessions: %v", err)
	}
	if !ok {
		release()
		return Err(CodeSessionNotFound, "session %s 不在对话 %s", cmd.SessionID, cmd.DialogID)
	}
	isMain := sval(rec["parent_id"]) == ""
	now := c.nowStr()
	if err := sdb.Table("sessions").UpdateIn(cmd.SessionID, func(cur data.Record) data.Record {
		if len(cur) == 0 {
			return nil
		}
		cur["title"] = cmd.Title
		cur["updated_at"] = now
		return cur
	}); err != nil {
		release()
		return Err(CodeBackend, "改会话标题: %v", err)
	}
	release()

	if isMain { // 主 session = 对话标题的真源 → 同步目录副本
		cat, relCat, err := c.catalog()
		if err != nil {
			return Err(CodeBackend, "打开 prjusr: %v", err)
		}
		defer relCat()
		if err := cat.Table("dialogs").UpdateIn(cmd.DialogID, func(r data.Record) data.Record {
			if len(r) == 0 {
				return nil
			}
			r["title"] = cmd.Title
			r["updated_at"] = now
			return r
		}); err != nil {
			return Err(CodeBackend, "同步 dialogs.title: %v", err)
		}
	}
	c.emit(Event{Kind: EventTitleChanged, DialogID: cmd.DialogID, SessionID: cmd.SessionID, Title: cmd.Title, IsDialogTitle: isMain})
	return nil
}

// DeleteDialog 删对话：目录行先删，再删库文件（两者都幂等）。
func (c *Component) DeleteDialog(cmd DeleteDialogCmd) error {
	if cmd.DialogID == "" {
		return Err(CodeDialogRequired, "dialog_id required")
	}
	cat, release, err := c.catalog()
	if err != nil {
		return Err(CodeBackend, "打开 prjusr: %v", err)
	}
	_, existed, err := getDialogRow(cat, cmd.DialogID)
	if err != nil {
		release()
		return Err(CodeBackend, "读 dialogs 行: %v", err)
	}
	if existed {
		if err := cat.Table("dialogs").Delete(cmd.DialogID); err != nil && !isNotFound(err) {
			release()
			return Err(CodeBackend, "删 dialogs 行: %v", err)
		}
	}
	release()
	if err := data.DeleteDialog(c.prjUsrPath, cmd.DialogID); err != nil {
		return Err(CodeBackend, "删对话库: %v", err)
	}
	if existed {
		c.emit(Event{Kind: EventDialogDeleted, DialogID: cmd.DialogID})
	}
	return nil
}

// ListDialogs 列对话（按 updated_at 降序、dialog_id 升序稳定）。
func (c *Component) ListDialogs() ([]DialogRow, error) {
	cat, release, err := c.catalog()
	if err != nil {
		return nil, Err(CodeBackend, "打开 prjusr: %v", err)
	}
	defer release()
	out := make([]DialogRow, 0, 16)
	if err := cat.Table("dialogs").ForEach(func(key string, rec data.Record) error {
		out = append(out, DialogRow{
			DialogID:  firstNonEmpty(sval(rec["dialog_id"]), key),
			Title:     sval(rec["title"]),
			Kind:      sval(rec["kind"]),
			UpdatedAt: sval(rec["updated_at"]),
		})
		return nil
	}); err != nil {
		return nil, Err(CodeBackend, "列 dialogs: %v", err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].DialogID < out[j].DialogID
	})
	return out, nil
}

// CreateSession 建子会话（`parent_id` 必填且须在同一对话内；每库恒 1 个主 session）。
func (c *Component) CreateSession(cmd CreateSessionCmd) (CreateSessionRes, error) {
	if cmd.DialogID == "" {
		return CreateSessionRes{}, Err(CodeDialogRequired, "dialog_id required")
	}
	if cmd.ParentID == "" {
		return CreateSessionRes{}, Err(CodeMainLimit, "一个对话只能有 1 个主 session；新建子会话须给 parent_id")
	}
	if err := validateParticipants(cmd.Participants); err != nil {
		return CreateSessionRes{}, err
	}
	cat, release, err := c.catalog()
	if err != nil {
		return CreateSessionRes{}, Err(CodeBackend, "打开 prjusr: %v", err)
	}
	_, exists, err := getDialogRow(cat, cmd.DialogID)
	release()
	if err != nil {
		return CreateSessionRes{}, Err(CodeBackend, "读 dialogs 行: %v", err)
	}
	if !exists {
		return CreateSessionRes{}, Err(CodeDialogNotFound, "对话不存在: %s", cmd.DialogID)
	}

	sdb, relDB, err := c.openDialog(cmd.DialogID)
	if err != nil {
		return CreateSessionRes{}, c.mapOpenErr(err, cmd.DialogID)
	}
	_, parentOK, err := sessionByID(sdb, cmd.ParentID)
	relDB()
	if err != nil {
		return CreateSessionRes{}, Err(CodeBackend, "读父会话: %v", err)
	}
	if !parentOK {
		return CreateSessionRes{}, Err(CodeSessionNotFound, "父会话 %s 不在对话 %s", cmd.ParentID, cmd.DialogID)
	}

	sessionID := cmd.SessionID
	if sessionID == "" {
		sessionID = mustID()
	}
	kind := firstNonEmpty(cmd.Kind, "sub_chat")
	if err := c.writeSession(cmd.DialogID, sessionID, firstNonEmpty(cmd.Title, sessionID), kind, cmd.ParentID, cmd.Participants); err != nil {
		return CreateSessionRes{}, err
	}
	c.emit(Event{Kind: EventSessionCreated, DialogID: cmd.DialogID, SessionID: sessionID, ParentID: cmd.ParentID, SessionKind: kind})
	return CreateSessionRes{DialogID: cmd.DialogID, SessionID: sessionID}, nil
}

// ListSessions 列某对话的全部会话行（含主 + 子，按 created_at 升序）。
func (c *Component) ListSessions(cmd ListSessionsCmd) (ListSessionsRes, error) {
	if cmd.DialogID == "" {
		return ListSessionsRes{}, Err(CodeDialogRequired, "dialog_id required")
	}
	sdb, release, err := c.openDialog(cmd.DialogID)
	if err != nil {
		return ListSessionsRes{}, c.mapOpenErr(err, cmd.DialogID)
	}
	defer release()
	out := make([]SessionRow, 0, 8)
	if err := sdb.Table("sessions").ForEach(func(key string, rec data.Record) error {
		out = append(out, SessionRow{
			SessionID: firstNonEmpty(sval(rec["session_id"]), key),
			Title:     sval(rec["title"]),
			Kind:      sval(rec["kind"]),
			ParentID:  sval(rec["parent_id"]),
			CreatedAt: sval(rec["created_at"]),
			UpdatedAt: sval(rec["updated_at"]),
		})
		return nil
	}); err != nil {
		return ListSessionsRes{}, Err(CodeBackend, "列 sessions: %v", err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].SessionID < out[j].SessionID
	})
	return ListSessionsRes{DialogID: cmd.DialogID, Sessions: out}, nil
}

// ── 内部 ───────────────────────────────────────────────────────

// writeSession 在对话库内写一行 session（库文件不存在则创建）。
func (c *Component) writeSession(dialogID, sessionID, title, kind, parentID string, ps []Participant) error {
	if sessionID == "" {
		return Err(CodeBackend, "session_id 为空")
	}
	sdb, release, err := c.openDialog(dialogID)
	if err != nil {
		return Err(CodeBackend, "打开对话库: %v", err)
	}
	defer release()
	now := c.nowStr()
	rec := data.Record{
		"session_id": sessionID,
		"title":      title,
		"kind":       kind,
		"parent_id":  parentID,
		"created_at": now,
		"updated_at": now,
	}
	if len(ps) > 0 {
		rec["participants"] = ps
	}
	if err := sdb.Table("sessions").Insert(sessionID, rec); err != nil {
		return Err(CodeBackend, "写 session: %v", err)
	}
	return nil
}

// mapOpenErr 把「库打开失败」映射为带码错误（2G §3.3：报错上抛，不回落 prjusr）。
func (c *Component) mapOpenErr(err error, dialogID string) error {
	if CodeOf(err) != "" {
		return err
	}
	return Err(CodeBackend, "打开对话库 %s: %v", dialogID, err)
}

// validateTitle 校验标题（≤128 字符、不含控制字符；空 = 未命名，允许）。
func validateTitle(title string) error {
	if len([]rune(title)) > 128 {
		return Err(CodeInvalid, "标题超长（≤128 字符）")
	}
	for _, r := range title {
		if unicode.IsControl(r) {
			return Err(CodeInvalid, "标题不得含控制字符")
		}
	}
	return nil
}

// mustID 生成 UUID v4（失败 → 空串，由调用方 writeSession 拦截）。
func mustID() string {
	id, err := data.NewDialogID()
	if err != nil {
		return ""
	}
	return id
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
