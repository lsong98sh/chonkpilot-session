// errors.go —— 组件错误码（2G §4.4；P1a 追加 `E_MAIN_LIMIT`，见 2G §4.4 订正）。
package component

import (
	"errors"
	"fmt"
)

// 错误码（字符串码 → 由 adapter / facade 映射成 MQ `errors{code}`）。
const (
	CodeDialogNotFound   = "E_DIALOG_NOT_FOUND"
	CodeSessionNotFound  = "E_SESSION_NOT_FOUND"
	CodeDialogRequired   = "E_DIALOG_REQUIRED"
	CodeTurnClosed       = "E_TURN_CLOSED"
	CodeTurnNotFound     = "E_TURN_NOT_FOUND"
	CodeParticipantLimit = "E_PARTICIPANT_LIMIT"
	// CodeMainLimit：一个对话库恒有且仅有 1 个主 session（parent_id == ""）；
	// 再次创建根会话 / 删除主会话时返回（2G §4.4 追加）。
	CodeMainLimit = "E_MAIN_LIMIT"
	// CodeInvalid：入参校验失败（标题超长 / 含控制字符等；2G §4.4 追加）。
	CodeInvalid = "E_INVALID"
	// CodeBackend：库打开 / 写入失败（**不回落** prjusr，2G §3.3）。
	CodeBackend = "E_BACKEND"
)

// CodedError 是带错误码的组件错误。
type CodedError struct {
	Code string
	Msg  string
}

func (e *CodedError) Error() string {
	if e.Msg == "" {
		return e.Code
	}
	return e.Code + ": " + e.Msg
}

// Err 构造带码错误。
func Err(code, format string, args ...any) error {
	return &CodedError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// CodeOf 返回错误的码（非 CodedError → ""）。
func CodeOf(err error) string {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}
