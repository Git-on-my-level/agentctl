package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const nativeOwnerCancelGrace = 5 * time.Second
const nativeOwnerSettleTimeout = nativeOwnerCancelGrace + time.Second

type nativeEventWriteError struct {
	stage string
	cause error
}

func (e *nativeEventWriteError) Error() string { return "native event write failed at " + e.stage }
func (e *nativeEventWriteError) Unwrap() error { return e.cause }
func nativeEventWriteStage(err error) string {
	var failure *nativeEventWriteError
	if errors.As(err, &failure) {
		return failure.stage
	}
	return "append_native_event"
}

// Never include the journal error text: validation errors may contain native
// metadata. The stage, event kind and sequence identify the failed write.
func nativeJournalErrorClass(err error) string {
	switch {
	case errors.Is(err, store.ErrEventValidation):
		return "event_validation"
	case errors.Is(err, store.ErrBusy):
		return "busy"
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrTerminalConflict):
		return "conflict"
	case errors.Is(err, store.ErrCorrupt):
		return "corrupt"
	case errors.Is(err, store.ErrPermission), errors.Is(err, store.ErrReadOnly):
		return "permission"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	default:
		return "write"
	}
}

func nativeLaunchFailure(err error, state adapter.State) adapter.Result {
	result := adapter.Result{State: state, Error: err.Error()}
	var ae *adapter.AdapterError
	if errors.As(err, &ae) && ae.Details["diagnostic_code"] == "zcode_read_only_unavailable" {
		code := "zcode_read_only_unavailable"
		if ae.Details["reason_code"] == adapter.ZCodeReasonProjectConfigPresent {
			code = "zcode_read_only_project_config_present"
		}
		if result.Data == nil {
			result.Data = map[string]any{}
		}
		result.Data["diagnostic_code"] = code
	}
	return result
}

// Cancel has already reaped the owned process. Recover its actual result and
// terminalize on every owner error, including a failed event write. A permanently
// unwritable journal cannot be repaired here: report that explicitly, with the
// execution id, rather than asserting that a terminal state was recorded.
func (a *app) settleNativeOwnerError(c common, runtime adapter.Adapter, ref adapter.SourceRef, id ids.ExecutionID, noStore bool, problem *output.Error, cancelErr error) *output.Error {
	problem.WithDetail("execution_id", id.String())
	if cancelErr != nil {
		return problem.WithDetail("cleanup", "unproven").WithDetail("terminal_recorded", false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeOwnerSettleTimeout)
	defer cancel()
	type waitResult struct {
		result adapter.Result
		err    error
	}
	// Some adapters finish reaping even after context cancellation. Bound the
	// owner response independently; the buffered channel lets a late Wait exit.
	finished := make(chan waitResult, 1)
	go func() {
		result, err := runtime.Wait(ctx, ref)
		finished <- waitResult{result, err}
	}()
	var result adapter.Result
	select {
	case <-ctx.Done():
		return problem.WithDetail("cleanup", "result_unavailable").WithDetail("terminal_recorded", false)
	case waited := <-finished:
		if ctx.Err() != nil || waited.err != nil || !terminalAdapterState(waited.result.State) {
			return problem.WithDetail("cleanup", "result_unavailable").WithDetail("terminal_recorded", false)
		}
		result = waited.result
	}
	problem.WithDetail("child_reaped", true)
	if result.State == adapter.StateCancelled {
		code, _ := problem.Details["diagnostic_code"].(string)
		if code != "native_event_write_failed" {
			code = "native_owner_failed"
		}
		result.Error = "native owner failed; child reaped"
		if result.Data == nil {
			result.Data = map[string]any{}
		}
		result.Data["diagnostic_code"] = code
	}
	journal, current, openProblem := a.openExecutionWriteOwned(c, id)
	if openProblem != nil {
		return problem.WithDetail("terminal_recorded", false).WithDetail("terminal_write_error", openProblem.Code)
	}
	defer journal.Close()
	terminal, err := a.finalizeResultConverging(context.Background(), c, journal, current, result, a.now().UTC(), noStore)
	if err != nil {
		return problem.WithDetail("terminal_recorded", false).WithDetail("terminal_write_error", nativeJournalErrorClass(err))
	}
	_ = journal.Close()
	a.anxRegisterTerminal(c, terminal.ID)
	return problem.WithDetail("terminal_recorded", true).WithDetail("state", terminal.State)
}

// This is an explicit repair, not cancellation inferred from a missing native
// session. Hold the journal writer lock while proving identity and committing.
func (a *app) orphanNativeIfGone(ctx context.Context, renderer output.Renderer, c common, id ids.ExecutionID) *output.Error {
	journal, problem := a.openWrite(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	current, err := journal.GetExecution(ctx, id)
	if err != nil {
		return mapStoreError("read orphan candidate", err)
	}
	if current.Authority != model.AuthorityNative {
		return output.NewError(output.CodeInvalidState, "orphan repair requires native authority", false)
	}
	if current.State.Terminal() {
		return writeExecution(renderer, current, "cancel")
	}
	host, err := journal.HostID(ctx)
	if err != nil {
		return mapStoreError("read journal host", err)
	}
	now := a.now().UTC()
	identity, recorded := recordedLaunch(current)
	if current.OriginHostID != host || !recorded || runnerLeaseActive(current, now) || current.NativeRecoveryParked() {
		return output.NewError(output.CodeInvalidState, "native owner absence cannot be proved", false)
	}
	proof := a.proveProcess(identity)
	if !proof.Gone {
		return output.NewError(output.CodeInvalidState, "recorded native process is alive or unproven", false).WithDetail("evidence", proof)
	}
	terminal, err := commitOwnerLost(ctx, journal, current, proof, observationAge(current, now), now)
	if err != nil {
		return mapStoreError("record owner lost", err)
	}
	_ = journal.Close()
	a.anxRegisterTerminal(c, terminal.ID)
	return writeExecution(renderer, terminal, "cancel")
}
