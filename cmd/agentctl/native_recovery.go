package main

import (
	"context"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// parkNativeFailure never replays prompts or tool calls. The caller has reaped
// the native child; the original recipe and checkpoint stay on this execution.
func (a *app) parkNativeFailure(journal *store.Journal, execution model.Execution, result adapter.Result) (model.Execution, bool, error) {
	if execution.Authority != model.AuthorityNative || execution.Delegation == nil || result.State != adapter.StateFailed ||
		(execution.Adapter != "codex" && execution.Adapter != "zcode") {
		return execution, false, nil
	}
	if nativeSessionID(execution) == "" {
		return execution, false, nil // no exact checkpoint exists to resume
	}
	plan := execution.Delegation.NativePlan
	if plan == nil {
		return execution, false, nil
	}
	if _, err := adapter.FailedContinuationArgv(execution.Adapter, plan.Recipe(), plan.PromptDelivery, nativeSessionID(execution)); err != nil {
		return execution, false, nil // e.g. ZCode's disposable read-only HOME
	}
	failure := buildOutcome(execution, result, a.now().UTC(), false).Failure
	if failure == nil || !failure.Retryable || (failure.Kind != "network" && failure.Kind != "capacity") {
		return execution, false, nil
	}
	now := a.now().UTC()
	state := "paused_" + failure.Kind
	execution.State, execution.Liveness, execution.SourceState = model.StateAttention, model.LivenessBlocked, &state
	execution.UpdatedAt = now
	execution.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}
	execution.RecordProgress(now)
	event, canonical, err := syntheticEvent(execution, model.EventAttention, execution.State,
		map[string]any{"reason": state, "failure": failure, "recovery": "explicit_native_continue", "native_process_exited": true}, state, now)
	if err != nil {
		return execution, false, err
	}
	updated, _, _, err := journal.CommitObservedEvent(context.Background(), execution, execution.Revision, event, canonical)
	return updated, true, err
}
