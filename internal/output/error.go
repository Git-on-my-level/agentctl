package output

import "fmt"

type Code string

const (
	CodeUsage                  Code = "usage"
	CodeUnsupportedSchema      Code = "unsupported_schema"
	CodeNotFound               Code = "not_found"
	CodeCursorExpired          Code = "cursor_expired"
	CodeAmbiguousReference     Code = "ambiguous_reference"
	CodeCapabilityUnavailable  Code = "capability_unavailable"
	CodeDependencyUnavailable  Code = "dependency_unavailable"
	CodeAuthenticationRequired Code = "authentication_required"
	CodeAuthorizationDenied    Code = "authorization_denied"
	CodeConflict               Code = "conflict"
	CodeInvalidState           Code = "invalid_state"
	CodeTimeout                Code = "timeout"
	CodeAttentionRequired      Code = "attention_required"
	CodeExecutionFailed        Code = "execution_failed"
	CodeRemoteFailure          Code = "remote_failure"
	CodeExecutionCancelled     Code = "execution_cancelled"
	CodeExecutionUnknown       Code = "execution_unknown"
	CodeUnknownState           Code = "unknown_state"
	CodeInternal               Code = "internal"
)

var exitCodes = map[Code]int{
	CodeUsage: 2, CodeUnsupportedSchema: 2, CodeNotFound: 3, CodeCursorExpired: 3, CodeAmbiguousReference: 4,
	CodeCapabilityUnavailable: 5, CodeDependencyUnavailable: 6, CodeAuthenticationRequired: 7, CodeAuthorizationDenied: 7,
	CodeConflict: 8, CodeInvalidState: 8, CodeTimeout: 9, CodeAttentionRequired: 10, CodeExecutionFailed: 11, CodeRemoteFailure: 11,
	CodeExecutionCancelled: 12, CodeExecutionUnknown: 13, CodeUnknownState: 13, CodeInternal: 70,
}

type Error struct {
	Code        Code           `json:"code"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	ExitCode    int            `json:"exit_code"`
	Details     map[string]any `json:"details"`
	NextActions []NextAction   `json:"next_actions"`
	Cause       error          `json:"-"`
}

func NewError(code Code, message string, retryable bool) *Error {
	exit, ok := exitCodes[code]
	if !ok {
		code = CodeInternal
		exit = 70
	}
	return &Error{Code: code, Message: message, Retryable: retryable, ExitCode: exit, Details: map[string]any{}, NextActions: []NextAction{}}
}
func Wrap(code Code, message string, retryable bool, cause error) *Error {
	e := NewError(code, message, retryable)
	e.Cause = cause
	return e
}
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
func (e *Error) WithDetail(key string, value any) *Error { e.Details[key] = value; return e }
func (e *Error) WithActions(actions ...NextAction) *Error {
	e.NextActions = append(e.NextActions, actions...)
	return e
}
func ExitCodeFor(code Code) int {
	if value, ok := exitCodes[code]; ok {
		return value
	}
	return 70
}

type ErrorDocument struct {
	OK            bool   `json:"ok"`
	SchemaVersion int    `json:"schema_version"`
	Error         *Error `json:"error"`
}
