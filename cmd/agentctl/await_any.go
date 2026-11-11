package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

// await-any is the fan-in form of await: it watches a set of executions and
// returns the first one that needs the caller. See docs/design/lane-fan-in.md.

const (
	awaitAnyMaxExecutions     = 256
	awaitAnyRemoteConcurrency = 8
	awaitAnyRemoteScanBudget  = 4 * time.Second
	awaitAnyRemoteProbeBudget = 3 * time.Second
	awaitAnyKindTerminal      = "terminal"
	awaitAnyKindAttention     = "attention"
	awaitAnyKindStall         = "stall"
	awaitAnyKindIntegrity     = "integrity"
)

type awaitAnyOptions struct {
	watch               func(ids.ExecutionID) *output.Error
	executionIDs        []ids.ExecutionID
	labelPrefix         string
	kinds               map[string]bool
	since               time.Time
	timeout             time.Duration
	includeAcknowledged bool
	noAcknowledge       bool
}

type awaitAnyMatch struct {
	execution model.Execution
	kind      string
	triggerAt time.Time
}

type awaitAnyRemoteScan struct {
	next     int
	deferred int
	problems map[ids.ExecutionID]*output.Error
}

func (s *awaitAnyRemoteScan) warnings() []output.Warning {
	keys := make([]string, 0, len(s.problems))
	for id := range s.problems {
		keys = append(keys, id.String())
	}
	sort.Strings(keys)
	warnings := []output.Warning{}
	for _, key := range keys {
		problem := s.problems[ids.ExecutionID(key)]
		// Keep diagnostics and provider prose out of public scan warnings.
		warnings = append(warnings, output.Warning{Code: "await_any_remote_probe_failed", Message: "Remote status probe failed; retry with per-execution await", Details: map[string]any{
			"execution_id": key, "remote": problem.Details["remote"], "code": problem.Code, "await_argv": []string{"agentctl", "await", key},
		}})
	}
	if s.deferred > 0 {
		warnings = append(warnings, output.Warning{Code: "await_any_remote_scan_partial", Message: "Remote scan budget exhausted; remaining rows resume on the next scan", Details: map[string]any{"deferred": s.deferred}})
	}
	return warnings
}

func (s *awaitAnyRemoteScan) deadlineError() *output.Error {
	if len(s.problems) == 0 {
		return nil
	}
	// Warnings are sorted by local ID; retain the original error and per-id
	// action for the first failure, with every host's failure in the envelope.
	var first ids.ExecutionID
	for id := range s.problems {
		if first.IsZero() || id.String() < first.String() {
			first = id
		}
	}
	return s.problems[first].WithDetail("await_any_deadline_elapsed", true)
}

type awaitAnyProbeResult struct {
	execution       model.Execution
	receipt         remoteReceipt
	problem         *output.Error
	scanInterrupted bool
}

// A fixed worker pool limits SSH fan-out. Resume at the first scan-interrupted
// row so every row gets a full probe window eventually. Join all workers
// (including SSH cancellation/reaping) before returning.
func awaitAnyProbeRemote(ctx context.Context, candidates []model.Execution, start int, probe func(context.Context, model.Execution) (remoteReceipt, *output.Error)) ([]awaitAnyProbeResult, int) {
	if len(candidates) == 0 {
		return nil, 0
	}
	scanCtx, cancel := context.WithTimeout(ctx, awaitAnyRemoteScanBudget)
	defer cancel()
	scanDeadline, _ := scanCtx.Deadline()
	results := make([]awaitAnyProbeResult, len(candidates))
	var mu sync.Mutex
	var workers sync.WaitGroup
	scheduled := 0
	for range min(awaitAnyRemoteConcurrency, len(candidates)) {
		workers.Go(func() {
			for {
				mu.Lock()
				if scanCtx.Err() != nil || scheduled == len(candidates) {
					mu.Unlock()
					return
				}
				i := scheduled
				scheduled++
				mu.Unlock()
				execution := candidates[(start+i)%len(candidates)]
				probeCtx, cancel := context.WithTimeout(scanCtx, awaitAnyRemoteProbeBudget)
				probeDeadline, _ := probeCtx.Deadline()
				receipt, problem := probe(probeCtx, execution)
				interrupted := problem != nil && probeCtx.Err() != nil && !probeDeadline.Before(scanDeadline)
				cancel()
				results[i] = awaitAnyProbeResult{execution: execution, receipt: receipt, problem: problem, scanInterrupted: interrupted}
			}
		})
	}
	workers.Wait()
	resume := scheduled
	for i, result := range results[:scheduled] {
		if result.scanInterrupted {
			resume = i
			break
		}
	}
	return results[:scheduled], (start + resume) % len(candidates)
}

func awaitAnyUsage() *output.Error {
	return output.NewError(output.CodeUsage, "usage: agentctl await-any (--execution ID ... | --label-prefix prefix) [--timeout duration|--no-timeout] [--kinds terminal,attention] [--since RFC3339] [--include-acknowledged] [--no-acknowledge]", false)
}

func parseAwaitAnyArgs(args []string) (awaitAnyOptions, *output.Error) {
	options := awaitAnyOptions{kinds: map[string]bool{awaitAnyKindTerminal: true, awaitAnyKindAttention: true}}
	seen := map[ids.ExecutionID]bool{}
	timeoutSet, noTimeout, kindsSet := false, false, false
	for i := 0; i < len(args); i++ {
		take := func() (string, *output.Error) {
			if i+1 >= len(args) {
				return "", output.NewError(output.CodeUsage, args[i]+" requires a value", false)
			}
			i++
			return args[i], nil
		}
		switch args[i] {
		case "--execution":
			value, problem := take()
			if problem != nil {
				return options, problem
			}
			for _, raw := range splitValues(value) {
				id, err := ids.ParseExecutionID(raw)
				if err != nil {
					return options, output.Wrap(output.CodeUsage, "invalid execution reference", false, err).WithDetail("ref", raw)
				}
				if !seen[id] {
					seen[id] = true
					options.executionIDs = append(options.executionIDs, id)
				}
			}
		case "--label-prefix":
			value, problem := take()
			if problem != nil {
				return options, problem
			}
			if options.labelPrefix != "" {
				return options, output.NewError(output.CodeUsage, "--label-prefix may be given once", false)
			}
			if !subscription.ValidLabelPrefix(value) {
				return options, output.NewError(output.CodeUsage, "label prefix must match ^[a-z][a-z0-9_.-]{0,127}$", false).WithDetail("label_prefix", value)
			}
			options.labelPrefix = value
		case "--timeout":
			value, problem := take()
			if problem != nil {
				return options, problem
			}
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				return options, output.NewError(output.CodeUsage, "timeout must be a positive Go duration", false)
			}
			options.timeout, timeoutSet = parsed, true
		case "--no-timeout":
			noTimeout = true
		case "--kinds", "--kind":
			value, problem := take()
			if problem != nil {
				return options, problem
			}
			if !kindsSet {
				options.kinds = map[string]bool{}
				kindsSet = true
			}
			values := splitValues(value)
			if len(values) == 0 {
				return options, output.NewError(output.CodeUsage, "--kinds requires terminal, attention, or both", false)
			}
			for _, kind := range values {
				switch kind {
				case awaitAnyKindTerminal, awaitAnyKindAttention:
					options.kinds[kind] = true
				case awaitAnyKindStall:
					return options, output.NewError(output.CodeCapabilityUnavailable, "stall detection is specified but not implemented in this build", false).WithDetail("diagnostic_code", "stall_unavailable").WithDetail("kind", kind)
				default:
					return options, output.NewError(output.CodeUsage, "unknown await-any kind", false).WithDetail("kind", kind)
				}
			}
		case "--since":
			value, problem := take()
			if problem != nil {
				return options, problem
			}
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return options, output.NewError(output.CodeUsage, "--since must be an RFC3339 timestamp", false).WithDetail("since", value)
			}
			options.since = parsed.UTC()
		case "--include-acknowledged":
			options.includeAcknowledged = true
		case "--no-acknowledge":
			options.noAcknowledge = true
		default:
			return options, output.NewError(output.CodeUsage, "unknown await-any flag", false).WithDetail("flag", args[i])
		}
	}
	if (len(options.executionIDs) == 0) == (options.labelPrefix == "") {
		return options, output.NewError(output.CodeUsage, "await-any requires exactly one of --execution or --label-prefix", false)
	}
	if len(options.executionIDs) > awaitAnyMaxExecutions {
		return options, output.NewError(output.CodeUsage, fmt.Sprintf("await-any accepts at most %d executions", awaitAnyMaxExecutions), false)
	}
	if timeoutSet && noTimeout {
		return options, output.NewError(output.CodeUsage, "--timeout and --no-timeout are mutually exclusive", false)
	}
	return options, nil
}

func (o awaitAnyOptions) selector() map[string]any {
	if o.labelPrefix != "" {
		return map[string]any{"label_prefix": o.labelPrefix}
	}
	values := make([]string, 0, len(o.executionIDs))
	for _, id := range o.executionIDs {
		values = append(values, id.String())
	}
	return map[string]any{"execution_ids": values}
}

func (o awaitAnyOptions) kindList() []string {
	values := make([]string, 0, len(o.kinds))
	for kind := range o.kinds {
		values = append(values, kind)
	}
	sort.Strings(values)
	return values
}

func hasLabelPrefix(labels []string, prefix string) bool {
	for _, label := range labels {
		if strings.HasPrefix(label, prefix) {
			return true
		}
	}
	return false
}

func (a *app) awaitAny(ctx context.Context, renderer output.Renderer, c common, args []string) int {
	options, problem := parseAwaitAnyArgs(args)
	if problem != nil {
		if problem.Code == output.CodeUsage {
			problem.WithActions(output.NextAction{Label: "Discover command usage", Argv: []string{"agentctl", "help", "await-any"}, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
		}
		return a.fail(renderer, problem)
	}
	// Only a terminal watcher with no timestamp exclusion proves that every
	// currently running selected execution can wake this collector.
	waiters := map[ids.ExecutionID]func(){}
	defer func() {
		for _, closeWaiter := range waiters {
			closeWaiter()
		}
	}()
	if options.kinds[awaitAnyKindTerminal] && options.since.IsZero() {
		options.watch = func(id ids.ExecutionID) *output.Error {
			if _, registered := waiters[id]; registered {
				return nil
			}
			closeWaiter, problem := a.registerWaiter(c, id)
			if problem != nil {
				return problem
			}
			waiters[id] = closeWaiter
			return nil
		}
	}
	var deadline time.Time
	if options.timeout > 0 {
		deadline = a.now().Add(options.timeout)
	}
	cancelled := func() int {
		return a.fail(renderer, output.Wrap(output.CodeExecutionCancelled, "await-any cancelled", false, ctx.Err()).WithDetail("selector", options.selector()))
	}
	lastWatched := 0
	remoteScan := awaitAnyRemoteScan{problems: map[ids.ExecutionID]*output.Error{}}
	var poll time.Duration
	for {
		if ctx.Err() != nil {
			return cancelled()
		}
		// One scan (open and reads) is bounded by the wait's deadline, with
		// waitOpenFloor of grace so the final poll can still complete.
		scanCtx, cancelScan := a.waitOpenContext(ctx, deadline)
		matches, watched, opened, problem := a.awaitAnyScan(scanCtx, c, options, &remoteScan)
		scanExpired := scanCtx.Err() != nil
		cancelScan()
		if ctx.Err() != nil {
			return cancelled()
		}
		if problem == nil && len(matches) == 0 && len(remoteScan.problems) == 0 && scanExpired && !deadline.IsZero() {
			// No observed winner or remote error can explain the expiry.
			problem = output.NewError(output.CodeTimeout, "await-any scan exceeded its deadline", true)
		}
		if problem != nil {
			if ctx.Err() != nil {
				return cancelled()
			}
			if !deadline.IsZero() && scanExpired {
				// Neither a busy journal nor a long scan may hold the wait past
				// --timeout.
				message := "await-any deadline elapsed during the scan"
				if !opened {
					message = "await-any deadline elapsed while the journal was busy"
				}
				return a.fail(renderer, output.NewError(output.CodeTimeout, message, true).
					WithDetail("selector", options.selector()).WithDetail("kinds", options.kindList()).WithDetail("watched", max(lastWatched, watched)))
			}
			return a.fail(renderer, problem)
		}
		poll = nextAwaitPoll(poll, watched != lastWatched)
		lastWatched = watched
		if len(matches) > 0 {
			renderer = renderer.WithWarnings(remoteScan.warnings()...)
			match := matches[0]
			if match.execution.RemoteForwarded() && match.kind == awaitAnyKindTerminal {
				// Status already proved terminal: the state wait is over.
				// Collection gets its journal-busy budget independently of
				// --timeout, while parent-context cancellation still applies.
				collected, problem := a.waitRemoteTerminal(ctx, c, match.execution, []string{"--no-timeout"}, time.Time{})
				if problem != nil && problem.Details["remote_terminal_collected"] != true {
					return a.fail(renderer, problem)
				}
				match.execution = collected
			}
			return a.deliverAwaitAny(ctx, renderer, c, options, match, len(matches)-1, watched)
		}
		if !deadline.IsZero() && !a.now().Before(deadline) {
			if probeProblem := remoteScan.deadlineError(); probeProblem != nil {
				return a.fail(renderer.WithWarnings(remoteScan.warnings()...), probeProblem)
			}
			return a.fail(renderer, output.NewError(output.CodeTimeout, "await-any deadline elapsed", true).
				WithDetail("selector", options.selector()).WithDetail("kinds", options.kindList()).WithDetail("watched", watched))
		}
		pause := awaitPollPause(poll, 0, false)
		if !deadline.IsZero() {
			if remaining := deadline.Sub(a.now()); remaining < pause {
				pause = remaining
			}
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return cancelled()
		case <-timer.C:
		}
	}
}

// awaitAnyScan returns matches ordered by trigger time, then execution ID.
// Exact-ID reads share one handle; label queries use advisory pages with the
// handle released between pages. ctx bounds the whole scan. opened distinguishes
// journal contention from a deadline reached after a successful open.
func (a *app) awaitAnyScan(ctx context.Context, c common, options awaitAnyOptions, remoteScan *awaitAnyRemoteScan) (matches []awaitAnyMatch, watched int, opened bool, problem *output.Error) {
	matches, watched, problem = a.awaitAnyScanJournal(ctx, c, options, &opened, remoteScan)
	return matches, watched, opened, problem
}

func (a *app) awaitAnyScanJournal(ctx context.Context, c common, options awaitAnyOptions, opened *bool, remoteScan *awaitAnyRemoteScan) ([]awaitAnyMatch, int, *output.Error) {
	matches := []awaitAnyMatch{}
	watched := 0
	remoteCandidates := []model.Execution{}
	waiterCandidates := []ids.ExecutionID{}
	skipReconciled := options.labelPrefix != "" && !options.includeAcknowledged
	visit := func(journal *store.Journal, candidates []model.Execution) error {
		watched += len(candidates)
		if options.labelPrefix != "" && watched > awaitAnyMaxExecutions {
			return output.NewError(output.CodeUsage, "await-any label prefix matches too many executions; narrow the prefix or use explicit IDs", false).
				WithDetail("label_prefix", options.labelPrefix).WithDetail("candidate_limit", awaitAnyMaxExecutions)
		}
		var index store.AcknowledgementIndex
		if skipReconciled && options.kinds[awaitAnyKindTerminal] {
			var err error
			index, err = journal.AcknowledgementIndexFor(ctx, candidates)
			if err != nil {
				return mapStoreError("read acknowledgements", err)
			}
		}
		for _, execution := range candidates {
			if err := ctx.Err(); err != nil {
				return output.Wrap(output.CodeTimeout, "await-any scan interrupted", true, err)
			}
			if options.watch != nil && !execution.State.Terminal() {
				waiterCandidates = append(waiterCandidates, execution.ID)
			}
			if execution.RemoteForwarded() && execution.Observation.Integrity != model.IntegrityConflicted {
				refresh := !execution.State.Terminal()
				if !refresh {
					// A terminal launch receipt has not yet collected the
					// remote outcome/integrity. Only collection writes this
					// small event marker; do not load answer bodies to test it.
					_, collected, err := journal.LatestEventOfKind(ctx, execution.ID, model.EventTerminal)
					if err != nil {
						return mapStoreError("read remote collection marker", err)
					}
					refresh = !collected
				}
				if refresh {
					remoteCandidates = append(remoteCandidates, execution)
					continue
				}
			}
			match, ok, problem := awaitAnyClassify(ctx, journal, options, execution, skipReconciled, index)
			if problem != nil {
				return problem
			}
			if ok {
				matches = append(matches, match)
			}
		}
		return nil
	}
	if options.labelPrefix != "" {
		path, err := a.journalPath(c)
		if err != nil {
			return nil, 0, output.Wrap(output.CodeInternal, "resolve journal path", false, err)
		}
		reader := store.Reader{Open: func(ctx context.Context, ro bool) (*store.Journal, error) {
			j, err := openJournalWithBusyBudget(ctx, path, store.Options{ReadOnly: ro}, c.journalBusyBudget > 0, c.journalBusyBudget)
			if err == nil {
				*opened = true
			}
			return j, err
		}}
		if err := reader.WalkExecutionsByLabelPrefix(ctx, options.labelPrefix, visit); err != nil {
			if problem, ok := err.(*output.Error); ok {
				return nil, 0, problem
			}
			return nil, 0, mapStoreError("list awaited executions", err)
		}
	} else {
		journal, problem := a.openReadContext(ctx, c)
		if problem != nil {
			return nil, 0, problem
		}
		*opened = true
		candidates := []model.Execution{}
		for _, id := range options.executionIDs {
			execution, err := journal.GetExecution(ctx, id)
			if err != nil {
				journal.Close()
				return nil, 0, mapStoreError("read awaited execution", err).WithDetail("execution_id", id.String())
			}
			candidates = append(candidates, execution)
		}
		err := visit(journal, candidates)
		journal.Close()
		if err != nil {
			return nil, 0, err.(*output.Error)
		}
	}

	// Validate the entire selector before publishing waiter evidence. Registration
	// performs filesystem I/O only after all journal handles close, preserving the
	// candidate bound's error precedence and short shared-lock holds.
	for _, id := range waiterCandidates {
		if err := ctx.Err(); err != nil {
			return nil, watched, output.Wrap(output.CodeTimeout, "await-any waiter registration interrupted", true, err)
		}
		if problem := options.watch(id); problem != nil {
			return nil, watched, problem
		}
	}
	// All journal handles are closed before SSH. Status is observational: only
	// the eventual winner is collected or acknowledged, including remotely.
	results, next := awaitAnyProbeRemote(ctx, remoteCandidates, remoteScan.next, func(probeCtx context.Context, execution model.Execution) (remoteReceipt, *output.Error) {
		return remoteObservation(probeCtx, execution, "status", []string{"--journal-busy-budget", c.journalBusyBudget.String()})
	})
	remoteScan.next = next
	remoteScan.deferred = len(remoteCandidates) - len(results)
	for _, result := range results {
		if result.scanInterrupted {
			remoteScan.deferred++
		}
	}
	// Drop errors for rows that no longer need probing, retaining errors for
	// deferred rows until a later scan can observe them successfully.
	current := map[ids.ExecutionID]bool{}
	for _, execution := range remoteCandidates {
		current[execution.ID] = true
	}
	for id := range remoteScan.problems {
		if !current[id] {
			delete(remoteScan.problems, id)
		}
	}
	for _, result := range results {
		execution, receipt, problem := result.execution, result.receipt, result.problem
		if problem != nil {
			remoteScan.problems[execution.ID] = problem.WithActions(output.NextAction{Label: "Wait for this remote execution", Argv: []string{"agentctl", "await", execution.ID.String()}, Mutates: true, SideEffectClass: output.LocalOperationalWrite})
			continue
		}
		delete(remoteScan.problems, execution.ID)
		execution.State = receipt.State
		if receipt.Observation != nil {
			execution.Observation.Integrity = receipt.Observation.Integrity
			execution.Observation.ObservedAt = receipt.Observation.ObservedAt
		}
		if receipt.Liveness != "" {
			execution.Liveness = receipt.Liveness
		}
		if !receipt.UpdatedAt.IsZero() {
			execution.UpdatedAt = receipt.UpdatedAt
		}
		execution.TerminalAt = receipt.TerminalAt
		if execution.State.Terminal() && execution.TerminalAt == nil {
			at := execution.UpdatedAt
			execution.TerminalAt = &at
		}
		// Newly observed terminals have not been collected locally. The
		// acknowledgement index applies once a terminal is stored locally.
		match, ok, problem := awaitAnyClassify(ctx, nil, options, execution, false, store.AcknowledgementIndex{})
		if problem != nil {
			return nil, watched, problem
		}
		if ok {
			matches = append(matches, match)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].triggerAt.Equal(matches[j].triggerAt) {
			return matches[i].triggerAt.Before(matches[j].triggerAt)
		}
		return matches[i].execution.ID.String() < matches[j].execution.ID.String()
	})
	return matches, watched, nil
}

func awaitAnyClassify(ctx context.Context, journal *store.Journal, options awaitAnyOptions, execution model.Execution, skipReconciled bool, index store.AcknowledgementIndex) (awaitAnyMatch, bool, *output.Error) {
	after := func(at time.Time) bool { return options.since.IsZero() || at.After(options.since) }
	if !options.kinds[awaitAnyKindTerminal] && !options.kinds[awaitAnyKindAttention] {
		return awaitAnyMatch{}, false, nil
	}
	if execution.Observation.Integrity == model.IntegrityConflicted {
		// Conflicted evidence is always actionable; report it under either kind.
		at := execution.Observation.ObservedAt
		if at.IsZero() {
			at = execution.UpdatedAt
		}
		if after(at) {
			return awaitAnyMatch{execution: execution, kind: awaitAnyKindIntegrity, triggerAt: at}, true, nil
		}
		return awaitAnyMatch{}, false, nil
	}
	switch {
	case execution.State.Terminal():
		if !options.kinds[awaitAnyKindTerminal] {
			return awaitAnyMatch{}, false, nil
		}
		if skipReconciled && !index.Unreconciled(execution) {
			return awaitAnyMatch{}, false, nil
		}
		at := execution.UpdatedAt
		if execution.TerminalAt != nil {
			at = *execution.TerminalAt
		}
		if !after(at) {
			return awaitAnyMatch{}, false, nil
		}
		return awaitAnyMatch{execution: execution, kind: awaitAnyKindTerminal, triggerAt: at}, true, nil
	case execution.State == model.StateAttention:
		if !options.kinds[awaitAnyKindAttention] {
			return awaitAnyMatch{}, false, nil
		}
		at := execution.UpdatedAt
		var err error
		if journal != nil {
			at, err = latestAttentionAt(ctx, journal, execution)
		}
		if err != nil {
			return awaitAnyMatch{}, false, mapStoreError("read attention events", err).WithDetail("execution_id", execution.ID.String())
		}
		if !after(at) {
			return awaitAnyMatch{}, false, nil
		}
		return awaitAnyMatch{execution: execution, kind: awaitAnyKindAttention, triggerAt: at}, true, nil
	}
	return awaitAnyMatch{}, false, nil
}

// latestAttentionAt returns the journal observation time of the newest
// (most recently appended) attention event, falling back to the envelope
// update time when the authority recorded attention without a journaled
// event. It reads backwards from the newest event, so history length does
// not bound it.
func latestAttentionAt(ctx context.Context, journal *store.Journal, execution model.Execution) (time.Time, error) {
	event, found, err := journal.LatestEventOfKind(ctx, execution.ID, model.EventAttention)
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return execution.UpdatedAt.UTC(), nil
	}
	return event.ObservedAt.UTC(), nil
}

func awaitAnyDetails(problem *output.Error, options awaitAnyOptions, match awaitAnyMatch, pending, watched int, acknowledged bool) *output.Error {
	return problem.WithDetail("matched_kind", match.kind).WithDetail("trigger_at", match.triggerAt.UTC().Format(time.RFC3339Nano)).
		WithDetail("pending_matches", pending).WithDetail("watched", watched).WithDetail("selector", options.selector()).
		WithDetail("acknowledged", acknowledged).WithDetail("labels", match.execution.Labels)
}

// deliverAwaitAny writes exactly one document for the chosen execution. Only
// that execution can be acknowledged; other matches stay untouched.
func (a *app) deliverAwaitAny(ctx context.Context, renderer output.Renderer, c common, options awaitAnyOptions, match awaitAnyMatch, pending, watched int) int {
	execution := match.execution
	acknowledge := !options.noAcknowledge && execution.State.Terminal() && match.kind == awaitAnyKindTerminal
	if match.kind == awaitAnyKindIntegrity {
		return a.fail(renderer, awaitAnyDetails(outcomeError(output.CodeUnknownState, "execution evidence is conflicted", execution), options, match, pending, watched, false))
	}
	var problem *output.Error
	switch execution.State {
	case model.StateCompleted:
		result := map[string]any{
			"execution_id": execution.ID.String(), "state": execution.State, "adapter": execution.Adapter, "labels": execution.Labels,
			"matched_kind": match.kind, "trigger_at": match.triggerAt.UTC().Format(time.RFC3339Nano), "pending_matches": pending,
			"watched": watched, "selector": options.selector(), "acknowledged": acknowledge,
		}
		lines := []output.Line{{Lead: execution.ID.String(), Fields: []output.Field{{Name: "state", Value: execution.State}, {Name: "matched_kind", Value: match.kind}, {Name: "pending_matches", Value: pending}}}}
		actions := []output.NextAction{{Label: "Read the completed result", Argv: []string{"agentctl", "result", execution.ID.String(), "--content"}, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}}}
		if err := renderer.Success(output.Success{Result: result, Lines: lines, NextActions: actions}); err != nil {
			return a.fail(renderer, output.Wrap(output.CodeInternal, "write output", false, err))
		}
		if acknowledge {
			if problem := a.acknowledgeExecution(ctx, c, execution.ID, store.AcknowledgementAwait); problem != nil {
				_, _ = fmt.Fprintln(a.stderr, "agentctl: terminal state delivered but acknowledgement failed:", problem)
			}
		}
		return 0
	case model.StateFailed:
		problem = outcomeError(output.CodeExecutionFailed, "execution failed", execution)
	case model.StateCancelled:
		problem = outcomeError(output.CodeExecutionCancelled, "execution was cancelled", execution)
	case model.StateOrphaned:
		problem = outcomeError(output.CodeExecutionUnknown, "execution is orphaned", execution)
	case model.StateAttention:
		return a.fail(renderer, awaitAnyDetails(outcomeError(output.CodeAttentionRequired, "execution requires attention", execution), options, match, pending, watched, false).
			WithDetail("authority", string(execution.Authority)).WithActions(attentionNextActions(renderer.Mode, execution)...))
	default:
		return a.fail(renderer, output.NewError(output.CodeInternal, "await-any matched a non-actionable state", false).WithDetail("execution_id", execution.ID.String()).WithDetail("state", execution.State))
	}
	problem = awaitAnyDetails(problem, options, match, pending, watched, acknowledge).
		WithActions(output.NextAction{Label: "Read the terminal result", Argv: []string{"agentctl", "result", execution.ID.String()}, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}})
	if !acknowledge {
		return a.fail(renderer, problem)
	}
	return a.deliverAwaitError(ctx, renderer, c, execution.ID, store.AcknowledgementAwait, problem)
}
