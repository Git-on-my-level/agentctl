// Package runtime connects adapter observations to the normalized execution
// journal. It owns integration policy, not native execution or persistence.
package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

// ErrorCode is stable across adapter and journal implementations.
type ErrorCode string

const (
	CodeCapabilityUnavailable  ErrorCode = "capability_unavailable"
	CodeDependencyUnavailable  ErrorCode = "dependency_unavailable"
	CodeAuthenticationRequired ErrorCode = "authentication_required"
	CodeNotFound               ErrorCode = "not_found"
	CodeTimeout                ErrorCode = "timeout"
	CodeConflict               ErrorCode = "conflict"
	CodeUnsafeObservation      ErrorCode = "unsafe_observation"
	CodeInvalidConfiguration   ErrorCode = "invalid_configuration"
	CodeExecutionFailed        ErrorCode = "execution_failed"
	CodeExecutionCancelled     ErrorCode = "execution_cancelled"
	CodeUnknownState           ErrorCode = "unknown_state"
	CodeUsage                  ErrorCode = "usage"
	CodeInternal               ErrorCode = "internal"
)

// Error is the runtime boundary's transport-neutral failure shape. Message is
// bounded operational text; adapter output and transcript material are never
// copied into Details.
type Error struct {
	Code      ErrorCode      `json:"code"`
	Operation string         `json:"operation,omitempty"`
	Adapter   string         `json:"adapter,omitempty"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
	Cause     error          `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return e.Message
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func wrapError(operation, adapterName string, err error) error {
	if err == nil {
		return nil
	}
	var existing *Error
	if errors.As(err, &existing) {
		copy := *existing
		if copy.Operation == "" {
			copy.Operation = operation
		}
		if copy.Adapter == "" {
			copy.Adapter = adapterName
		}
		return &copy
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &Error{Code: CodeTimeout, Operation: operation, Adapter: adapterName, Message: "operation timed out or was cancelled", Retryable: true, Cause: err}
	}
	var ae *adapter.AdapterError
	if errors.As(err, &ae) {
		code := ErrorCode(ae.Code)
		if code == "execution_unknown" {
			code = CodeUnknownState
		}
		return &Error{Code: code, Operation: operation, Adapter: adapterName, Message: bounded(ae.Message, 512), Retryable: ae.Retryable, Cause: err}
	}
	return &Error{Code: CodeInternal, Operation: operation, Adapter: adapterName, Message: fmt.Sprintf("%s failed", operation), Cause: err}
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
