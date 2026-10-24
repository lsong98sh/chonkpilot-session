// fork.go —— 对话派生（2G §5，SESSION-COMP-1 P4）。
//
// 组件侧只做「参数校验 + 存储原语调用 + 事件」：文件拷贝与 id 重映射的唯一实现 =
// `data.ForkDialog`（`chonkpilot-data/fork.go`；数据面门面同样调用它，不各写一套）。
//
// 机制（full）：拷文件（bbolt 备份 API）→ 单事务重映射 session 引用 → 写 dialogs 行。
// context：不拷文件，只写主 session 行 + 源主 session 的 snapshot。
package component

import (
	"errors"

	"github.com/chonkpilot/chonkpilot-data"
)

// Fork 档位（转发 data 常量，调用方不必区分两层）。
const (
	ForkModeFull    = data.ForkModeFull
	ForkModeContext = data.ForkModeContext
)

// ForkDialogCmd 派生一个对话（`DialogID` = 源对话，必填）。
type ForkDialogCmd struct {
	DialogID string
	Mode     string // "" → full
	Title    string // "" → 源主 session 标题
}

// ForkRes 是派生结果（新 dialog_id + 新主 session_id）。
type ForkRes struct {
	DialogID  string `json:"dialog_id"`
	SessionID string `json:"session_id"`
}

// ForkDialog 派生对话（见文件头机制说明）。
func (c *Component) ForkDialog(cmd ForkDialogCmd) (ForkRes, error) {
	if cmd.DialogID == "" {
		return ForkRes{}, Err(CodeDialogRequired, "dialog_id required")
	}
	mode := firstNonEmpty(cmd.Mode, ForkModeFull)
	if mode != ForkModeFull && mode != ForkModeContext {
		return ForkRes{}, Err(CodeInvalid, "fork mode 非法: %q（full|context）", mode)
	}
	res, err := data.ForkDialog(c.prjUsrPath, cmd.DialogID, mode, cmd.Title)
	if err != nil {
		switch {
		case errors.Is(err, data.ErrForkDialogNotFound):
			return ForkRes{}, Err(CodeDialogNotFound, "%v", err)
		case errors.Is(err, data.ErrForkNoMainSession), errors.Is(err, data.ErrForkNoSessions):
			return ForkRes{}, Err(CodeSessionNotFound, "%v", err)
		default:
			return ForkRes{}, Err(CodeBackend, "派生对话: %v", err)
		}
	}
	c.emit(Event{
		Kind: EventDialogCreated, DialogID: res.DialogID, SessionID: res.SessionID,
		Title: res.Title, SessionKind: res.Kind,
	})
	return ForkRes{DialogID: res.DialogID, SessionID: res.SessionID}, nil
}
