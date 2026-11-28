package main

import (
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

const (
	nativeApprovalWaitTimeout       = 10 * time.Minute
	nativeApprovalWaitDiagnostic    = "native_approval_wait_timeout"
	nativeCommandDeclinedDiagnostic = "native_command_declined"
)

func updateNativeApprovalWait(since *time.Time, events []adapter.Event, fallback time.Time) *time.Time {
	for _, event := range events {
		if approvalAttentionEvent(event) {
			if since == nil {
				observed := event.ObservedAt
				if observed.IsZero() {
					observed = fallback
				}
				value := observed
				since = &value
			}
			continue
		}
		if since != nil && nativeApprovalResumed(event) {
			since = nil
		}
	}
	return since
}

func nativeApprovalResumed(event adapter.Event) bool {
	if event.State != adapter.StateRunning || (event.Kind != "started" && event.Kind != "progress") {
		return false
	}
	sourceState := strings.ToLower(event.SourceState)
	switch sourceState {
	case "stderr", "health", "malformed_output":
		return false
	}
	return true
}

func approvalAttentionEvent(event adapter.Event) bool {
	if event.State != adapter.StateAttention {
		return false
	}
	kind, _ := event.Payload["attention_kind"].(string)
	if kind == "approval" || kind == "permission" {
		return true
	}
	state := strings.ToLower(event.SourceState)
	return strings.Contains(state, "approval") || strings.Contains(state, "permission")
}

func nativeApprovalWaitExpired(since *time.Time, now time.Time) bool {
	return since != nil && !now.Before(since.Add(nativeApprovalWaitTimeout))
}

func nativeApprovalWaitFailure(ref adapter.SourceRef, cancellationConfirmed bool) adapter.Result {
	return adapter.Result{
		Success:    false,
		State:      adapter.StateFailed,
		Error:      "Native command approval remained unresolved for 10 minutes; the execution was cancelled.",
		SessionRef: ref,
		Data: map[string]any{
			"diagnostic_code":          nativeApprovalWaitDiagnostic,
			"terminal_source_state":    nativeApprovalWaitDiagnostic,
			"attention_kind":           "permission",
			"approval_wait_seconds":    int64(nativeApprovalWaitTimeout / time.Second),
			"failure_category":         "permission",
			"process_cancel_confirmed": cancellationConfirmed,
			"repair_action":            "Review the native command permission policy, then retry with a new request key.",
		},
	}
}

func nativeCommandDeclined(result adapter.Result) bool {
	code, _ := result.Data["diagnostic_code"].(string)
	return code == nativeCommandDeclinedDiagnostic
}
