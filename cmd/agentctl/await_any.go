package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

// await-any is the fan-in form of await: it watches a set of executions and
// returns the first one that needs the caller. See docs/design/lane-fan-in.md.

const (
	awaitAnyPollInterval   = time.Second
	awaitAnyMaxExecutions  = 256
	awaitAnyKindTerminal   = "terminal"
	awaitAnyKindAttention  = "attention"
	awaitAnyKindStall      = "stall"
	awaitAnyKindIntegrity  = "integrity"
	awaitAnyAttentionPages = 50
)

type awaitAnyOptions struct {
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
	var deadline time.Time
	if options.timeout > 0 {
		deadline = a.now().Add(options.timeout)
	}
	cancelled := func() int {
		return a.fail(renderer, output.Wrap(output.CodeExecutionCancelled, "await-any cancelled", false, ctx.Err()).WithDetail("selector", options.selector()))
	}
	for {
		if ctx.Err() != nil {
			return cancelled()
		}
		matches, watched, problem := a.awaitAnyScan(ctx, c, options)
		if problem != nil {
			return a.fail(renderer, problem)
		}
		if len(matches) > 0 {
			return a.deliverAwaitAny(ctx, renderer, c, options, matches[0], len(matches)-1, watched)
		}
		if !deadline.IsZero() && !a.now().Before(deadline) {
			return a.fail(renderer, output.NewError(output.CodeTimeout, "await-any deadline elapsed", true).
				WithDetail("selector", options.selector()).WithDetail("kinds", options.kindList()).WithDetail("watched", watched))
		}
		timer := time.NewTimer(awaitAnyPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return cancelled()
		case <-timer.C:
		}
	}
}

// awaitAnyScan reads one journal snapshot and returns matches ordered by
// trigger time, then execution ID, so the choice is deterministic.
func (a *app) awaitAnyScan(ctx context.Context, c common, options awaitAnyOptions) ([]awaitAnyMatch, int, *output.Error) {
	journal, problem := a.openRead(c)
	if problem != nil {
		return nil, 0, problem
	}
	defer journal.Close()
	var candidates []model.Execution
	if options.labelPrefix != "" {
		all, err := journal.ListExecutions(ctx, false)
		if err != nil {
			return nil, 0, mapStoreError("list awaited executions", err)
		}
		for _, execution := range all {
			if hasLabelPrefix(execution.Labels, options.labelPrefix) {
				candidates = append(candidates, execution)
			}
		}
	} else {
		for _, id := range options.executionIDs {
			execution, err := journal.GetExecution(ctx, id)
			if err != nil {
				return nil, 0, mapStoreError("read awaited execution", err).WithDetail("execution_id", id.String())
			}
			candidates = append(candidates, execution)
		}
	}
	// Label scope means "new work under this prefix": terminals already
	// collected (or predating acknowledgement tracking) are skipped. Exact IDs
	// keep await's level-triggered semantics unless the caller opts out.
	skipReconciled := options.labelPrefix != "" && !options.includeAcknowledged
	var index store.AcknowledgementIndex
	if skipReconciled && options.kinds[awaitAnyKindTerminal] {
		var err error
		if index, err = journal.AcknowledgementIndex(ctx); err != nil {
			return nil, 0, mapStoreError("read acknowledgements", err)
		}
	}
	matches := []awaitAnyMatch{}
	for _, execution := range candidates {
		match, ok, problem := awaitAnyClassify(ctx, journal, options, execution, skipReconciled, index)
		if problem != nil {
			return nil, 0, problem
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
	return matches, len(candidates), nil
}

func awaitAnyClassify(ctx context.Context, journal *store.Journal, options awaitAnyOptions, execution model.Execution, skipReconciled bool, index store.AcknowledgementIndex) (awaitAnyMatch, bool, *output.Error) {
	after := func(at time.Time) bool { return options.since.IsZero() || at.After(options.since) }
	if !options.kinds[awaitAnyKindTerminal] && !options.kinds[awaitAnyKindAttention] {
		return awaitAnyMatch{}, false, nil
	}
	if execution.Observation.Integrity == model.IntegrityConflicted {
		// Conflicted evidence is always actionable; report it under either kind.
		if after(execution.UpdatedAt) {
			return awaitAnyMatch{execution: execution, kind: awaitAnyKindIntegrity, triggerAt: execution.UpdatedAt}, true, nil
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
		at, err := latestAttentionAt(ctx, journal, execution)
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
// attention event, falling back to the envelope update time when the
// authority recorded attention without a journaled event.
func latestAttentionAt(ctx context.Context, journal *store.Journal, execution model.Execution) (time.Time, error) {
	latest := time.Time{}
	var after uint64
	for page := 0; page < awaitAnyAttentionPages; page++ {
		events, err := journal.ListEvents(ctx, execution.ID, contracts.EventQuery{AfterSequence: after, Limit: 1000})
		if err != nil {
			return time.Time{}, err
		}
		for _, event := range events {
			if event.Kind == model.EventAttention && event.ObservedAt.After(latest) {
				latest = event.ObservedAt
			}
			after = event.Sequence
		}
		if len(events) < 1000 {
			break
		}
	}
	if latest.IsZero() {
		latest = execution.UpdatedAt
	}
	return latest.UTC(), nil
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
