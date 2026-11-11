package main

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const childrenUsage = "agentctl children [--execution exec-... | --provider name --native-session-id id] [--summary]"

func (a *app) children(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	execution, provider, session := "", "", ""
	summary := false
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if seen[flag] {
			return output.NewError(output.CodeUsage, childrenUsage, false)
		}
		seen[flag] = true
		switch flag {
		case "--summary":
			summary = true
		case "--execution", "--provider", "--native-session-id":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, childrenUsage, false)
			}
			i++
			switch flag {
			case "--execution":
				execution = args[i]
			case "--provider":
				provider = canonicalAdapterName(args[i])
			case "--native-session-id":
				session = args[i]
			}
		default:
			return output.NewError(output.CodeUsage, childrenUsage, false)
		}
	}
	if (seen["--execution"] && (seen["--provider"] || seen["--native-session-id"])) || (seen["--execution"] && execution == "") || ((seen["--provider"] || seen["--native-session-id"]) && (!identityProviderPattern.MatchString(provider) || !validNativeSessionID(session))) {
		return output.NewError(output.CodeUsage, childrenUsage, false)
	}
	// A single budget covers identity lookup, journal opens, and the whole scan.
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	report, problem := a.resolveCallerIdentity(ctx, c, execution, provider, session, identityResolutionOptions{failUnavailable: true})
	if problem != nil {
		if problem.Retryable || ctx.Err() != nil {
			return childrenQueryIncomplete()
		}
		return problem
	}
	scope := store.ChildrenScope{}
	if report.Execution.ID != nil && report.Execution.Confidence == "observed" {
		id, _ := ids.ParseExecutionID(*report.Execution.ID)
		scope.Parent = &id
	} else if report.Provider.ID != nil && report.NativeSession.ID != nil {
		scope.Provider, scope.NativeSessionID = *report.Provider.ID, *report.NativeSession.ID
	} else {
		return output.NewError(output.CodeInvalidState, "caller identity is unknown; provide --execution or --provider and --native-session-id", false).WithDetail("diagnostic_code", "children_identity_unknown")
	}
	path, err := a.journalPath(c)
	if err != nil {
		return output.Wrap(output.CodeDependencyUnavailable, "children journal unavailable", false, err)
	}
	reader := store.Reader{Open: func(ctx context.Context, readOnly bool) (*store.Journal, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return store.Open(path, store.Options{ReadOnly: true, LockTimeout: 50 * time.Millisecond})
	}}
	children, err := reader.Children(ctx, scope, a.now().UTC())
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, store.ErrBusy) || errors.Is(err, store.ErrChildrenBounded) {
			return childrenQueryIncomplete()
		}
		return mapStoreError("query children", err)
	}
	for i := range children {
		children[i].CollectArgv = childCollectArgv(c, children[i].ID)
	}

	terminal, running := 0, 0
	for _, child := range children {
		if child.State.Terminal() {
			terminal++
		} else {
			running++
		}
	}
	result := map[string]any{"schema_version": 1, "complete": true, "coverage": "retained_attributed_executions_only", "state_consistency": "live_per_page", "parent_execution_id": scope.Parent, "provider": scope.Provider, "native_session_id": scope.NativeSessionID, "count": len(children), "terminal_uncollected": terminal, "running_without_known_waiter": running, "waiter_evidence": "live_exact_await_and_state_subscriptions", "executions": children}
	if summary {
		result["executions"] = []store.ChildExecution{}
	}
	if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "children", Fields: []output.Field{{Name: "count", Value: strconv.Itoa(len(children))}, {Name: "terminal_uncollected", Value: terminal}, {Name: "running_without_known_waiter", Value: running}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write children", false, err)
	}
	return nil
}

func childrenQueryIncomplete() *output.Error {
	return output.NewError(output.CodeTimeout, "children query incomplete; retry without treating this as an empty result", true).WithDetail("diagnostic_code", "children_query_incomplete").WithDetail("complete", false)
}
