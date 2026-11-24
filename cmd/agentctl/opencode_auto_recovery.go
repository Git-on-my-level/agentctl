package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const (
	openCodeRecoveryAdmissionBudget = time.Minute
	openCodeRateLimitInstruction    = "Inspect the session history. If the task already finished, return its final answer. Otherwise reconcile any previous tool effects, then continue the task and return the required final answer."
	openCodeFinalAnswerInstruction  = "Your last turn ended without a final answer. Reply now with only your final answer in the required format."
)

// Let the native path return and reap the child before sleeping or admitting
// another turn. Any launch-scoped admission lease must be released here.
func (a *app) runNativeRecovering(ctx context.Context, renderer output.Renderer, c common, opts runOptions) *output.Error {
	var source model.Execution
	opts.scheduledRecovery = &source
	problem := a.runNativeOptions(ctx, renderer, c, nil, opts)
	if !source.ID.IsZero() {
		a.runOpenCodeRecovery(ctx, c, source)
		if problem == nil && ctx.Err() != nil {
			return interruptedRunError(ctx.Err(), source)
		}
	}
	return problem
}

// planOpenCodeRecovery runs only for a reaped child and returns a schedule to
// commit atomically with the physical outcome. The second result means a
// structured 429 has reached the automatic path, so exhaustion is a failure
// instead of a new indefinite capacity park.
func (a *app) planOpenCodeRecovery(c common, journal *store.Journal, source model.Execution, result adapter.Result) (*model.OpenCodeRecovery, bool) {
	if source.Authority != model.AuthorityNative || source.Adapter != "opencode" || result.State != adapter.StateFailed ||
		source.Delegation == nil || source.Delegation.NativePlan == nil || !verifiedOpenCodeExecution(source) ||
		nativeSessionID(source) == "" || c.contextFile != "" || a.getenv("MULTICA_TASK_ID") != "" {
		return nil, false
	}
	status, _ := nativeMetadataInt(result.Data["http_status"])
	kind := model.OpenCodeRateLimit
	if status != 429 || stringData(result.Data, "failure_category") != "capacity" ||
		localCompletionDiagnostic(stringData(result.Data, "diagnostic_code")) {
		if stringData(result.Data, "diagnostic_code") != "empty_terminal_result" {
			return nil, false
		}
		kind = model.OpenCodeFinalAnswer
	}
	plan := source.Delegation.NativePlan
	if _, err := adapter.FailedContinuationArgv(source.Adapter, plan.Recipe(), plan.PromptDelivery, nativeSessionID(source)); err != nil {
		return nil, false
	}
	if !a.openCodeRecoveryEnabled(c, kind) {
		return nil, false
	}
	autoCapacity := kind == model.OpenCodeRateLimit
	attempt := 1
	all, err := journal.ListExecutions(context.Background(), false)
	if err != nil {
		return nil, autoCapacity
	}
	root := continuationRoot(all, source)
	lineage := continuationSuccessors(all, root)
	for _, previous := range all {
		if previous.ID == root {
			lineage = append(lineage, previous)
			break
		}
	}
	for _, previous := range lineage {
		if r := previous.OpenCodeRecovery; r != nil && r.Kind == kind && nativeSessionID(previous) == nativeSessionID(source) {
			attempt++
		}
	}
	now := a.now().UTC()
	notBefore := now
	if autoCapacity {
		if attempt > model.OpenCodeRetryLimit {
			return nil, true
		}
		retryAfter, _ := nativeMetadataInt(result.Data["retry_after_ms"])
		if source.RateLimit != nil {
			retryAfter = max(retryAfter, source.RateLimit.RetryAfterMS)
		}
		// A longer provider embargo is outside this short quota recovery
		// path. Never schedule an earlier retry by truncating the embargo.
		if retryAfter > time.Hour.Milliseconds() {
			return nil, true
		}
		delay := a.openCodeRecoveryDelay
		if delay == nil {
			delay = func(attempt int, retryAfterMS int64) time.Duration {
				return openCodeBackoff(attempt, retryAfterMS, rand.N)
			}
		}
		notBefore = now.Add(delay(attempt, retryAfter))
	} else if attempt > 1 {
		return nil, false
	}
	expires := notBefore.Add(openCodeRecoveryAdmissionBudget)
	if source.DeadlineAt != nil && source.DeadlineAt.Before(expires) {
		expires = *source.DeadlineAt
	}
	if !expires.After(notBefore) {
		return nil, autoCapacity
	}
	return &model.OpenCodeRecovery{Kind: kind, Attempt: attempt, NotBefore: notBefore, ExpiresAt: expires}, autoCapacity
}

func openCodeBackoff(attempt int, retryAfterMS int64, jitter func(time.Duration) time.Duration) time.Duration {
	base := max(time.Minute, time.Duration(retryAfterMS)*time.Millisecond)
	return base*(1<<(attempt-1)) + jitter(base)
}

func (a *app) openCodeRecoveryEnabled(c common, kind model.OpenCodeRecoveryKind) bool {
	_, profile, problem := a.resolveProfile(c)
	if problem != nil {
		return false
	}
	if profile.Delegation == nil {
		return true
	}
	flag := profile.Delegation.OpenCodeRateLimitRetry
	if kind == model.OpenCodeFinalAnswer {
		flag = profile.Delegation.OpenCodeFinalAnswerFollowup
	}
	return flag == nil || *flag
}

func (a *app) runOpenCodeRecovery(ctx context.Context, c common, source model.Execution) {
	schedule := source.OpenCodeRecovery
	if source.DeadlineAt != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, *source.DeadlineAt)
		defer cancel()
	}
	defer a.closeOpenCodeRecovery(c, source.ID)
	if !a.waitOpenCodeRecovery(ctx, c, source) {
		return
	}
	if !a.openCodeRecoveryEnabled(c, schedule.Kind) || !a.claimOpenCodeRecovery(ctx, c, source.ID) {
		return
	}
	key := "agentctl-final-answer/" + source.ID.String()
	instruction := openCodeFinalAnswerInstruction
	if schedule.Kind == model.OpenCodeRateLimit {
		key = fmt.Sprintf("agentctl-rate-limit/%s/%d", source.ID, schedule.Attempt)
		instruction = openCodeRateLimitInstruction
	}
	bytes := []byte(instruction)
	prompt := &promptPayload{Bytes: bytes, Digest: sha256Digest(bytes), Source: "opencode_recovery"}
	opts := delegateOptions{authority: "native", continuationMode: "native", automaticRecovery: true}
	// A preflight refusal leaves the persisted physical failure as the result.
	// Once admitted, even a failed or cancelled successor is the next outcome.
	_ = a.continuePrepared(ctx, output.Renderer{Mode: output.JSON, Writer: io.Discard}, c, source.ID, key, opts, prompt, nil)
}

func (a *app) waitOpenCodeRecovery(ctx context.Context, c common, source model.Execution) bool {
	for {
		remaining := source.OpenCodeRecovery.NotBefore.Sub(a.now())
		if remaining <= 0 {
			return ctx.Err() == nil
		}
		timer := time.NewTimer(min(remaining, time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		journal, problem := a.openReadContext(ctx, c)
		if problem != nil {
			return false
		}
		current, err := journal.GetExecution(ctx, source.ID)
		journal.Close()
		if err != nil || !current.OpenCodeRecoveryPending(a.now()) {
			return false
		}
	}
}

func (a *app) claimOpenCodeRecovery(ctx context.Context, c common, id ids.ExecutionID) bool {
	journal, current, problem := a.openExecutionWrite(ctx, c, id)
	if problem != nil {
		return false
	}
	defer journal.Close()
	r := current.OpenCodeRecovery
	now := a.now().UTC()
	if !current.OpenCodeRecoveryPending(now) || r.Claimed || now.Before(r.NotBefore) {
		return false
	}
	r.Claimed = true
	current.UpdatedAt = now
	_, err := journal.UpdateExecution(ctx, current, current.Revision)
	return err == nil
}

func (a *app) closeOpenCodeRecovery(c common, id ids.ExecutionID) {
	// Expiry is the fallback when cleanup cannot reach the journal.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	journal, current, problem := a.openExecutionWrite(ctx, c, id)
	if problem != nil {
		return
	}
	defer journal.Close()
	_, _ = stopOpenCodeRecovery(ctx, journal, current, a.now().UTC())
}

func stopOpenCodeRecovery(ctx context.Context, journal *store.Journal, current model.Execution, now time.Time) (model.Execution, error) {
	if !current.OpenCodeRecoveryPending(now) {
		return current, nil
	}
	current.OpenCodeRecovery.Closed = true
	current.UpdatedAt = now
	return journal.UpdateExecution(ctx, current, current.Revision)
}

// followOpenCodeRecovery follows only owner-admitted edges, never a manual or
// child-collection continuation. Readers retain the original logical ID;
// outcome reads and acknowledgements target the physical terminal turn.
func (a *app) followOpenCodeRecovery(ctx context.Context, journal *store.Journal, original model.Execution) (model.Execution, ids.ExecutionID, error) {
	if original.OpenCodeRecovery == nil {
		return original, original.ID, nil
	}
	active := original
	chain := []ids.ExecutionID{original.ID}
	for active.OpenCodeRecovery != nil && active.SupersededBy != nil {
		next, err := journal.GetExecution(ctx, *active.SupersededBy)
		if err != nil {
			return model.Execution{}, "", err
		}
		if next.RetryOf == nil || *next.RetryOf != active.ID {
			break
		}
		if len(chain) >= model.OpenCodeRetryLimit+2 {
			return model.Execution{}, "", fmt.Errorf("%w: OpenCode recovery chain exceeds its turn limit", store.ErrCorrupt)
		}
		active = next
		chain = append(chain, active.ID)
	}
	physical := active.ID
	if active.RetryOf != nil && active.State == model.StateStarting && len(active.SourceBindings) == 0 && active.OwnerLaunch != nil {
		owner := active.OwnerLaunch
		if a.proveProcess(processIdentity{PID: owner.PID, StartedAt: owner.StartedAt, StartTicks: owner.StartTicks,
			PIDNamespaceIno: owner.PIDNamespaceIno, ProcfsPIDNamespaceIno: owner.ProcfsPIDNamespaceIno}).Gone {
			active.Liveness = model.LivenessUnreachable
		}
	}
	if active.OpenCodeRecoveryPending(a.now()) {
		state := "opencode_recovery_pending"
		active.State, active.Liveness = model.StateWaiting, model.LivenessUnknown
		active.TerminalAt, active.SourceState = nil, &state
		active.Observation.FreshForSeconds = nil
	}
	active.ID, active.Delegation, active.DeadlineAt = original.ID, original.Delegation, original.DeadlineAt
	active.Supersedes, active.SupersededBy, active.RetryOf = original.Supersedes, original.SupersededBy, original.RetryOf
	active.RecoveryChain = chain
	return active, physical, nil
}

func withRecoveryChain(problem *output.Error, execution model.Execution) *output.Error {
	if len(execution.RecoveryChain) != 0 {
		problem.WithDetail("recovery_chain", execution.RecoveryChain)
	}
	return problem
}

func parkedOpenCodeRateLimitFailure(e model.Execution) *nativeFailureView {
	if e.Adapter != "opencode" || !e.NativeRecoveryParked() || *e.SourceState != "paused_capacity" {
		return nil
	}
	return &nativeFailureView{Code: "provider_capacity", Kind: "capacity", Category: "capacity",
		Message: failureMessageOpenCodeRateLimit, Retryable: true}
}
