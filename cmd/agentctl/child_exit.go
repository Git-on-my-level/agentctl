package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// Registration is best effort and attempted once by the owner. A sidecar
// failure cannot refuse the primary native operation; query coverage stays unknown.
func (a *app) registerWaiter(ctx context.Context, c common, id ids.ExecutionID) func() {
	path, err := a.journalPath(c)
	if err != nil {
		return func() {}
	}
	waiter, err := store.RegisterWaiter(ctx, path, id)
	if err != nil {
		return func() {}
	}
	return func() { _ = waiter.Close() }
}

func childCollectArgv(c common, id ids.ExecutionID) []string {
	absolute := func(path string) string {
		if resolved, err := filepath.Abs(path); err == nil {
			return resolved
		}
		return path
	}

	argv := []string{"agentctl"}
	if c.journalPath != "" {
		argv = append(argv, "--journal", absolute(c.journalPath))
	}
	if c.configPath != "" {
		argv = append(argv, "--config", absolute(c.configPath))
	}
	if c.configBundle != "" {
		argv = append(argv, "--config-bundle", absolute(c.configBundle))
	}
	if c.profile != "" {
		argv = append(argv, "--profile", c.profile)
	}
	return append(argv, "await", id.String(), "--content")
}

// checkChildExit runs before the atomic terminal execution/outcome/event commit,
// borrowing its journal handle. Query failure remains explicit unknown coverage.
func (a *app) checkChildExit(ctx context.Context, c common, journal *store.Journal, execution model.Execution, now time.Time) model.Execution {
	if execution.Authority != model.AuthorityNative || execution.RemoteForwarded() || !execution.State.Terminal() || refusedAdmission(execution) {
		return execution
	}
	queryCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	check, err := journal.CheckChildCollection(queryCtx, store.ChildrenScope{Parent: &execution.ID}, now)
	children := check.Children
	execution.ChildExitRecheck = check.Recheck || err != nil
	if err == nil && len(children) == 0 {
		return execution
	}
	guard := &model.ChildExitGuard{CheckedAt: now, AutoContinue: "unavailable"}
	execution.ChildExitGuard = guard
	if err != nil {
		guard.CheckError = "children_query_incomplete"
		return execution
	}
	collectionScope := c
	collectionScope.journalPath = journal.Path()
	if collectionScope.configPath == "" {
		if path, err := configPath(c); err == nil {
			if _, err := os.Stat(path); err == nil {
				collectionScope.configPath = path
			}
		}
	}
	for i := range children {
		children[i].CollectArgv = childCollectArgv(collectionScope, children[i].ID)
	}
	execution.UncollectedChildren = children
	switch {
	case execution.AutoContinuationOf != nil:
		guard.AutoContinue = "suppressed_recursive"
	case execution.State != model.StateCompleted && execution.State != model.StateFailed:
		guard.AutoContinue = "parent_not_completed"
	default:
		if c.contextFile != "" {
			guard.Diagnostic = "continue_context_unpinned"
			break
		}
		_, profile, problem := a.resolveProfile(c)
		if problem != nil {
			guard.Diagnostic = "child_exit_config_unavailable"
			break
		}
		if profile.Delegation != nil && profile.Delegation.ChildExitAutoContinue != nil && !*profile.Delegation.ChildExitAutoContinue {
			guard.AutoContinue = "disabled"
			break
		}
		if execution.Delegation == nil || execution.Delegation.NativePlan == nil || nativeSessionID(execution) == "" {
			guard.Diagnostic = "continue_launch_or_session_unrecorded"
			break
		}
		plan := execution.Delegation.NativePlan
		var routeErr error
		if execution.State == model.StateFailed {
			if execution.Observation.Integrity != model.IntegrityVerified {
				guard.Diagnostic = "continue_recovery_unverified"
				break
			}
			_, routeErr = adapter.FailedContinuationArgv(execution.Adapter, plan.Recipe(), plan.PromptDelivery, nativeSessionID(execution))
		} else {
			_, routeErr = adapter.ContinuationArgv(execution.Adapter, plan.Recipe(), plan.PromptDelivery, nativeSessionID(execution))
		}
		if routeErr != nil {
			guard.Diagnostic = "continue_route_unavailable"
			break
		}
		// Hermes remains provider-unverified. ACP Devin routes validate loadSession
		// during the normal continuation launch and fail closed if it is absent.
		if execution.Adapter == "hermes" {
			guard.Diagnostic = "continue_route_unverified"
			break
		}
		guard.AutoContinue = "pending"
	}
	return execution
}

// finishNativeTurn runs only in the actual native owner, never keyed replay or
// read-only inspection. Claim before launch: crashes cannot consume a second
// automatic turn. Public continue admission still arbitrates competing turns.
func (a *app) finishNativeTurn(ctx context.Context, renderer output.Renderer, c common, execution model.Execution) *output.Error {
	if execution.ChildExitGuard == nil || execution.ChildExitGuard.AutoContinue != "pending" {
		return writeExecution(renderer, execution, "run")
	}
	// The native result is already committed. Local settlement must still run
	// after cancellation; the original context below forbids a new native turn.
	journal, current, problem := a.openExecutionWrite(context.Background(), c, execution.ID)
	if problem != nil {
		return problem
	}
	if current.ChildExitGuard == nil || current.ChildExitGuard.AutoContinue != "pending" {
		journal.Close()
		return writeExecution(renderer, current, "run")
	}
	guard := *current.ChildExitGuard
	if ctx.Err() != nil || (current.DeadlineAt != nil && !a.now().Before(*current.DeadlineAt)) {
		guard.AutoContinue = "parent_interrupted"
	} else {
		guard.AutoContinue = "attempted"
	}
	current.ChildExitGuard = &guard
	claimed, err := journal.UpdateExecution(context.Background(), current, current.Revision)
	journal.Close()
	if err != nil {
		return mapStoreError("claim child collection continuation", err)
	}
	if guard.AutoContinue != "attempted" {
		return writeExecution(renderer, claimed, "run")
	}
	prompt := childExitPrompt(claimed)
	sum := sha256.Sum256([]byte(prompt))
	payload := &promptPayload{Bytes: []byte(prompt), Digest: fmt.Sprintf("sha256:%x", sum), Source: "child_exit_guard"}
	opts := delegateOptions{authority: "native", continuationMode: "native", timeout: 30 * time.Minute}
	// The caller's deadline remains a hard cap, including automatic cleanup.
	if claimed.DeadlineAt != nil {
		opts.timeout = min(opts.timeout, time.Until(*claimed.DeadlineAt))
	}
	var captured bytes.Buffer
	problem = a.continuePrepared(ctx, output.Renderer{Mode: output.JSON, Writer: &captured}, c, claimed.ID, "agentctl-child-exit/"+claimed.ID.String(), opts, payload, &claimed.ID)
	var successor *ids.ExecutionID
	// Recover the admitted successor by its immutable provenance, including a
	// failed continuation. No retries or synthetic fallback sessions are launched.
	if problem == nil {
		var receipt struct {
			Result struct {
				ID ids.ExecutionID `json:"id"`
			} `json:"result"`
		}
		if jsonErr := json.Unmarshal(captured.Bytes(), &receipt); jsonErr == nil && !receipt.Result.ID.IsZero() {
			successor = &receipt.Result.ID
		}
	} else if raw, ok := problem.Details["execution_id"].(string); ok {
		if id, err := ids.ParseExecutionID(raw); err == nil && id != claimed.ID {
			successor = &id
		}
	}
	journal, current, openProblem := a.openExecutionWrite(context.Background(), c, claimed.ID)
	if openProblem != nil {
		return openProblem
	}
	if current.ChildExitGuard != nil {
		updatedGuard := *current.ChildExitGuard
		updatedGuard.ContinuationID = successor
		if problem != nil {
			updatedGuard.AutoContinue = "failed"
			updatedGuard.Diagnostic = "child_exit_continue_failed"
			if diagnostic, ok := problem.Details["diagnostic_code"].(string); ok && len(diagnostic) <= 128 {
				updatedGuard.Diagnostic = diagnostic
			}
		} else {
			updatedGuard.AutoContinue = "launched"
		}
		current.ChildExitGuard = &updatedGuard
		current, err = journal.UpdateExecution(context.Background(), current, current.Revision)
	}
	journal.Close()
	if err != nil {
		return mapStoreError("record child collection continuation", err)
	}
	return writeExecution(renderer, current, "run")
}

func childExitPrompt(execution model.Execution) string {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Your turn %s exited with child work that has no known collector. Collect these children before finishing. This is the only automatic continuation; it will not repeat. For failed children, inspect the delivered failure and report it. If work is still running, keep an await armed until it settles. Exact collection commands:\n", execution.ID)
	for _, child := range execution.UncollectedChildren {
		fmt.Fprintf(&prompt, "- %s (%s; %s): %s\n", child.ID, child.State, child.Reason, remoteCommand(child.CollectArgv...))
	}
	return prompt.String()
}

func childExitWarnings(e model.Execution) []output.Warning {
	if e.ChildExitGuard == nil {
		return nil
	}
	code, message := "uncollected_children", "this turn exited with outstanding child work; collect the named children"
	if e.ChildExitGuard.CheckError != "" {
		code, message = "children_query_incomplete", "child collection coverage could not be established; query children before considering this turn settled"
	}
	return []output.Warning{{Code: code, Message: message, Details: map[string]any{"uncollected_children": e.UncollectedChildren, "child_exit_guard": e.ChildExitGuard}}}
}
