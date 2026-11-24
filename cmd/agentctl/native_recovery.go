package main

import (
	"context"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// parkNativeFailure never replays prompts or tool calls. The caller has reaped
// the native child; the original recipe and checkpoint stay on this execution.
func (a *app) parkNativeFailure(c common, journal *store.Journal, execution model.Execution, result adapter.Result) (model.Execution, bool, error) {
	if execution.Authority != model.AuthorityNative || execution.Delegation == nil || result.State != adapter.StateFailed ||
		(execution.Adapter != "codex" && execution.Adapter != "zcode" && execution.Adapter != "opencode") {
		return execution, false, nil
	}
	if nativeSessionID(execution) == "" {
		return execution, false, nil // no exact checkpoint exists to resume
	}
	if execution.Adapter == "opencode" && !verifiedOpenCodeExecution(execution) {
		return execution, false, nil
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
	if execution.Adapter == "opencode" && openCodeCapacityFailure(result) {
		execution.RateLimit = parkedOpenCodeRetry(execution.RateLimit, result, now)
		execution = a.checkChildExit(context.Background(), c, journal, execution, now)
	}
	event, canonical, err := syntheticEvent(execution, model.EventAttention, execution.State,
		map[string]any{"reason": state, "failure": failure, "recovery": "explicit_native_continue", "native_process_exited": true}, state, now)
	if err != nil {
		return execution, false, err
	}
	updated, _, _, err := journal.CommitObservedEvent(context.Background(), execution, execution.Revision, event, canonical)
	return updated, true, err
}

func parkedOpenCodeRetry(previous *model.NativeRateLimit, result adapter.Result, now time.Time) *model.NativeRateLimit {
	terminalDelay, _ := nativeMetadataInt(result.Data["retry_after_ms"])
	if previous == nil && terminalDelay <= 0 {
		return nil
	}
	retry := model.NativeRateLimit{Attempt: 1, HTTPStatus: 429}
	if previous != nil {
		retry = *previous
	}
	if provider := stringData(result.Data, "provider_error_type"); provider != "" {
		retry.ProviderErrorType = provider
	}
	retry.RetryAfterMS = openCodeRetryFloorMS(terminalDelay, previous, now)
	delay := terminalDelay
	if retry.NextRetryAt == nil {
		delay = retry.RetryAfterMS
	}
	if delay > 0 {
		// Preserve the provider's millisecond bound without overflowing
		// time.Duration or the JSON timestamp range.
		const lastJSONSecond = 253402300799
		next := time.Unix(min(now.Unix()+delay/1000, lastJSONSecond), int64(now.Nanosecond())).UTC()
		if next.Unix() < lastJSONSecond {
			next = next.Add(time.Duration(delay%1000) * time.Millisecond)
		}
		if retry.NextRetryAt == nil || next.After(*retry.NextRetryAt) {
			retry.NextRetryAt = &next
		}
	}
	return &retry
}
