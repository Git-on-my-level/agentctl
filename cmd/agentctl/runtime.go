package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

type runOptions struct {
	adapter, cwd, idempotencyKey, issue, run string
	promptFile, promptDelivery               string
	executionID                              ids.ExecutionID
	labels                                   []string
	plan                                     bool
	background                               bool
	noStoreResult                            bool
	allowMissingResult                       bool
	allowUnreliableResult                    bool
	timeout                                  time.Duration
	timeoutSet, noTimeoutSet, promptStdin    bool
	argv                                     []string
}

type promptPayload struct {
	Bytes    []byte
	Digest   string
	Delivery string
	Source   string
	Path     string
}

const (
	nativeRunnerLeaseSeconds   = 30
	nativeRunnerHeartbeatEvery = 10 * time.Second
	nativeEventFreshness       = 30 * time.Second
	maxPromptBytes             = 8 << 20
)

func (a *app) runNative(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	opts, problem := parseRun(args)
	if problem != nil {
		return problem
	}
	prompt, problem := a.loadPrompt(opts)
	if problem != nil {
		return problem
	}
	if opts.background && !opts.plan {
		return a.runNativeBackground(ctx, renderer, c, args, opts, prompt)
	}
	if prompt != nil {
		if prompt.Delivery == "argv" {
			if !utf8.Valid(prompt.Bytes) || strings.IndexByte(string(prompt.Bytes), 0) >= 0 {
				return output.NewError(output.CodeUsage, "argv prompt must be valid UTF-8 without NUL bytes", false)
			}
			if len(prompt.Bytes) != 0 && prompt.Bytes[0] == '-' && !containsArg(opts.argv, "--") {
				opts.argv = append(opts.argv, "--")
			}
			opts.argv = append(opts.argv, string(prompt.Bytes))
		}
	}
	runtime, profileName, problem := a.runtimeAdapter(c, opts.adapter, opts.issue, opts.run)
	if problem != nil {
		return problem
	}
	if runtime.Name() == "cursor" && cursorPlanMode(opts.argv) && !opts.allowUnreliableResult {
		return output.NewError(output.CodeCapabilityUnavailable, "Cursor plan mode does not provide reliable one-shot terminal result semantics", false).
			WithDetail("adapter", "cursor").
			WithDetail("mode", "plan").
			WithDetail("diagnostic_code", "cursor_plan_result_unreliable").
			WithActions(output.NextAction{Label: "Use default Cursor agent mode", Argv: []string{"agentctl", "help", "run"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"omit Cursor --plan and --mode plan"}})
	}
	if !opts.allowMissingResult && !opts.noStoreResult {
		invocation := adapter.NegotiateInvocation(runtime.Manifest(), opts.argv, adapter.CapabilityResultContent)
		if problem := requireRunCapability(adapter.ProbeResult{Capabilities: []adapter.Capability{invocation}}, adapter.CapabilityResultContent); problem != nil {
			problem.Message = "exact invocation cannot reliably return result content"
			return problem.WithDetail("adapter", runtime.Name())
		}
	}
	operationCtx := ctx
	var operationCancel context.CancelFunc
	if opts.timeout > 0 {
		operationCtx, operationCancel = context.WithTimeout(ctx, opts.timeout)
		defer operationCancel()
	}
	probe, err := runtime.Probe(operationCtx, adapter.ProbeRequest{Executable: opts.argv[0], Argv: opts.argv, Profile: profileName, Timeout: 5 * time.Second, Fresh: true})
	if err != nil {
		return mapAdapterError("adapter probe failed", err)
	}
	if problem := requireRunCapability(probe, adapter.CapabilityLaunch); problem != nil {
		return problem.WithDetail("adapter", runtime.Name())
	}
	if !opts.allowMissingResult && !opts.noStoreResult {
		if problem := requireRunCapability(probe, adapter.CapabilityResultContent); problem != nil {
			problem.Message = "adapter cannot reliably return result content"
			problem.NextActions = append(problem.NextActions, output.NextAction{Label: "Inspect adapter viability", Argv: []string{"agentctl", "capabilities", runtime.Name(), "--require", "launch,result_content"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
			return problem.WithDetail("adapter", runtime.Name())
		}
	}
	if opts.plan {
		result := map[string]any{"adapter": runtime.Name(), "executable": opts.argv[0], "argument_count": len(opts.argv) - 1, "profile": profileName, "side_effect_class": output.ExternalSideEffect, "probe": probe, "writes_local_state": true, "stores_result": !opts.noStoreResult, "timeout": timeoutDescription(opts), "background": opts.background, "labels": opts.labels}
		if prompt != nil {
			result["prompt"] = map[string]any{"source": prompt.Source, "delivery": prompt.Delivery, "bytes": len(prompt.Bytes), "sha256": prompt.Digest}
		}
		if !opts.executionID.IsZero() {
			result["execution_id"] = opts.executionID.String()
		}
		if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "plan", Fields: []output.Field{{Name: "command", Value: "run"}, {Name: "adapter", Value: runtime.Name()}, {Name: "executable", Value: opts.argv[0]}, {Name: "arguments", Value: len(opts.argv) - 1}, {Name: "side_effect_class", Value: output.ExternalSideEffect}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	}
	// Do not create a durable execution when the operation was already
	// interrupted before launch preparation began. Once an execution is
	// created below, all later interruptions are terminalized durably.
	if err := operationCtx.Err(); err != nil {
		return interruptedRunError(err, model.Execution{})
	}
	journal, problem := a.openWrite(c)
	if problem != nil {
		return problem
	}
	now := a.now().UTC()
	fresh := int(probe.FreshFor / time.Second)
	var deadlineAt *time.Time
	if deadline, ok := operationCtx.Deadline(); ok {
		deadline = deadline.UTC()
		deadlineAt = &deadline
	}
	execution := model.Execution{ID: opts.executionID, Authority: model.AuthorityNative, Adapter: runtime.Name(), Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateStarting, Liveness: model.LivenessUnknown, SourceBindings: []model.SourceBinding{}, Capabilities: capabilitySnapshot(probe), Labels: append([]string(nil), opts.labels...), Supersedes: []ids.ExecutionID{}, DeadlineAt: deadlineAt, Observation: model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityUnknown, ObservedAt: now, FreshForSeconds: &fresh}}
	mutation := contracts.MutationKey{}
	if opts.idempotencyKey != "" {
		digest, dErr := mutationDigest(runtime.Name(), opts.cwd, opts.argv, opts.labels, opts.noStoreResult, prompt)
		if dErr != nil {
			journal.Close()
			return output.Wrap(output.CodeInternal, "canonicalize run idempotency inputs", false, dErr)
		}
		mutation = contracts.MutationKey{Scope: "execution:run", Key: opts.idempotencyKey, InputDigest: digest}
	}
	// The operation context can be cancelled concurrently with this small local
	// commit. Complete it under a live context so every created execution either
	// launches or reaches one durable terminal state.
	execution, reused, err := journal.CreateExecution(context.Background(), execution, mutation)
	if err != nil {
		journal.Close()
		return mapStoreError("create execution", err)
	}
	if reused {
		journal.Close()
		return writeExecution(renderer, execution, "run")
	}
	// bbolt takes a process-wide writer lock. Do not hold it for the lifetime of
	// a native child: status/subscription commands and the callback supervisor
	// must be able to observe the execution while it is running. Release it
	// before signaling background readiness so the parent can prove the durable
	// execution exists without racing this worker's startup lock.
	journal.Close()
	if emitBackgroundReady(renderer.Writer, execution.ID) {
		// The parent owns the one startup document. Keep the detached worker's
		// eventual terminal rendering off a pipe that closes when the parent exits.
		renderer.Writer = io.Discard
	}
	launchCtx := operationCtx
	launchRequest := adapter.LaunchRequest{Argv: opts.argv, Cwd: opts.cwd, Context: contextInput(c), DiscoveryWindow: 250 * time.Millisecond, StartOnly: true}
	if prompt != nil && prompt.Delivery == "stdin" {
		launchRequest.Stdin = prompt.Bytes
	}
	launch, err := runtime.Launch(launchCtx, launchRequest)
	if err != nil {
		writeJournal, current, openProblem := a.openExecutionWrite(context.Background(), c, execution.ID)
		if openProblem != nil {
			return openProblem
		}
		failureState := adapter.StateFailed
		if launchInterrupted(err) {
			failureState = adapter.StateCancelled
		}
		failed, finalizeErr := finalizeResult(context.Background(), writeJournal, current, adapter.Result{Success: false, State: failureState, Error: err.Error()}, a.now().UTC(), opts.noStoreResult)
		writeJournal.Close()
		if finalizeErr != nil {
			return mapStoreError("record launch failure outcome", finalizeErr)
		}
		return mapAdapterError("launch failed", err).WithDetail("execution_id", failed.ID.String())
	}
	// Native launches are same-process owned. Any return path must reap the
	// child, including journal or renderer failures, so foreground fan-out and
	// lock contention cannot leak a process group.
	defer func() {
		_ = runtime.Cancel(context.Background(), adapter.CancelRequest{Ref: launch.Session.Ref, Signal: "term", Grace: 5 * time.Second})
	}()
	writeJournal, current, openProblem := a.openExecutionWriteOwned(c, execution.ID)
	if openProblem != nil {
		return openProblem
	}
	execution, err = applySession(writeJournal, current, launch.Session, a.now().UTC())
	if err != nil {
		writeJournal.Close()
		return mapStoreError("record launched execution", err)
	}
	if err := appendSynthetic(context.Background(), writeJournal, execution, model.EventStarted, execution.State, map[string]any{"accepted": true}, "launch"); err != nil {
		writeJournal.Close()
		return mapStoreError("record started event", err)
	}
	writeJournal.Close()
	cursor := ""
	nextRunnerHeartbeat := a.now().Add(nativeRunnerHeartbeatEvery)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !a.now().Before(nextRunnerHeartbeat) {
			writeJournal, current, openProblem = a.openExecutionWriteOwned(c, execution.ID)
			if openProblem != nil {
				return openProblem
			}
			execution, err = refreshRunnerLease(writeJournal, current, a.now().UTC())
			writeJournal.Close()
			if err != nil {
				return mapStoreError("refresh native runner lease", err)
			}
			nextRunnerHeartbeat = a.now().Add(nativeRunnerHeartbeatEvery)
		}
		nativeEvents, eventErr := runtime.Events(launchCtx, adapter.EventsRequest{Ref: launch.Session.Ref, Cursor: cursor})
		if eventErr == nil && len(nativeEvents) != 0 {
			var terminal bool
			execution, terminal, openProblem = a.recordNativeEvents(c, execution, nativeEvents, &cursor)
			if openProblem != nil {
				return openProblem
			}
			if terminal {
				return writeExecution(renderer, execution, "run")
			}
			nextRunnerHeartbeat = a.now().Add(nativeRunnerHeartbeatEvery)
		}
		result, resultErr := runtime.Result(launchCtx, adapter.ResultRequest{Ref: launch.Session.Ref})
		if resultErr == nil && terminalAdapterState(result.State) {
			// The structured stream may announce terminal state just before the
			// process exits. Reap the child before returning so a foreground run
			// never leaves an orphan behind. Keep renewing the runner lease while
			// waiting because some native CLIs perform lengthy exit cleanup after
			// emitting their terminal result.
			var waitProblem *output.Error
			execution, waited, waitErr, waitProblem := a.waitForNativeExitWithLease(launchCtx, c, runtime, launch.Session.Ref, execution)
			if waitProblem != nil {
				return waitProblem
			}
			if waitErr == nil {
				result = waited
			} else if !errors.Is(waitErr, context.Canceled) && !errors.Is(waitErr, context.DeadlineExceeded) {
				return mapAdapterError("wait for native process exit", waitErr).WithDetail("execution_id", execution.ID.String())
			}
			// Drain records that arrived between the last poll and terminal
			// result observation. The normalized terminal event remains owned by
			// CommitTerminalOutcome below, so native terminal records are skipped.
			if pending, pendingErr := runtime.Events(context.Background(), adapter.EventsRequest{Ref: launch.Session.Ref, Cursor: cursor}); pendingErr == nil && len(pending) != 0 {
				var terminal bool
				execution, terminal, openProblem = a.recordNativeEvents(c, execution, pending, &cursor)
				if openProblem != nil {
					return openProblem
				}
				if terminal {
					return writeExecution(renderer, execution, "run")
				}
			}
			writeJournal, current, openProblem = a.openExecutionWriteOwned(c, execution.ID)
			if openProblem != nil {
				return openProblem
			}
			execution, err = finalizeResultConverging(context.Background(), writeJournal, current, result, a.now().UTC(), opts.noStoreResult)
			writeJournal.Close()
			if err != nil {
				return mapStoreError("record terminal result", err)
			}
			if launchCtx.Err() != nil && execution.State != model.StateCompleted {
				return interruptedRunError(launchCtx.Err(), execution)
			}
			return writeExecution(renderer, execution, "run")
		}
		select {
		case <-launchCtx.Done():
			last, _ := runtime.Result(context.Background(), adapter.ResultRequest{Ref: launch.Session.Ref})
			if !terminalAdapterState(last.State) {
				_ = runtime.Cancel(context.Background(), adapter.CancelRequest{Ref: launch.Session.Ref, Signal: "term", Grace: 5 * time.Second})
				last, _ = runtime.Result(context.Background(), adapter.ResultRequest{Ref: launch.Session.Ref})
			}
			// Cancellation or a deadline can race with the final structured
			// records. Preserve any assistant/diagnostic events already parsed
			// before committing the normalized terminal outcome.
			if pending, pendingErr := runtime.Events(context.Background(), adapter.EventsRequest{Ref: launch.Session.Ref, Cursor: cursor}); pendingErr == nil && len(pending) != 0 {
				var terminal bool
				execution, terminal, openProblem = a.recordNativeEvents(c, execution, pending, &cursor)
				if openProblem != nil {
					return openProblem
				}
				if terminal {
					return writeExecution(renderer, execution, "run")
				}
			}
			if last.State != adapter.StateCompleted {
				message := "agentctl run cancelled"
				if errors.Is(launchCtx.Err(), context.DeadlineExceeded) {
					message = "agentctl run deadline elapsed"
				}
				last = adapter.Result{Success: false, State: adapter.StateCancelled, Error: message, SessionRef: launch.Session.Ref}
			}
			writeJournal, current, openProblem := a.openExecutionWriteOwned(c, execution.ID)
			if openProblem != nil {
				return openProblem
			}
			execution, err = finalizeResultConverging(context.Background(), writeJournal, current, last, a.now().UTC(), opts.noStoreResult)
			writeJournal.Close()
			if err != nil {
				return mapStoreError("record timed out execution", err)
			}
			if execution.State == model.StateCompleted {
				return writeExecution(renderer, execution, "run")
			}
			return interruptedRunError(launchCtx.Err(), execution)
		case <-ticker.C:
		}
	}
}

func (a *app) recordNativeEvents(c common, execution model.Execution, nativeEvents []adapter.Event, cursor *string) (model.Execution, bool, *output.Error) {
	journal, current, problem := a.openExecutionWriteOwned(c, execution.ID)
	if problem != nil {
		return execution, false, problem
	}
	defer journal.Close()
	execution = current
	if execution.State.Terminal() {
		return execution, true, nil
	}
	for _, nativeEvent := range nativeEvents {
		if nativeEvent.Cursor != "" {
			*cursor = nativeEvent.Cursor
		}
		if nativeEvent.Kind == "terminal" {
			continue
		}
		updated, appendErr := appendNativeEvent(context.Background(), journal, execution, nativeEvent)
		if errors.Is(appendErr, store.ErrConflict) {
			latest, reloadErr := journal.GetExecution(context.Background(), execution.ID)
			if reloadErr != nil {
				return execution, false, mapStoreError("reload execution after native event conflict", reloadErr)
			}
			if latest.State.Terminal() {
				return latest, true, nil
			}
			execution = latest
			updated, appendErr = appendNativeEvent(context.Background(), journal, execution, nativeEvent)
		}
		if errors.Is(appendErr, store.ErrTerminalConflict) {
			latest, reloadErr := journal.GetExecution(context.Background(), execution.ID)
			if reloadErr != nil {
				return execution, false, mapStoreError("reload terminal execution", reloadErr)
			}
			return latest, true, nil
		}
		if appendErr != nil {
			return execution, false, mapStoreError("record native event", appendErr)
		}
		execution = updated
	}
	return execution, false, nil
}

type nativeWaitOutcome struct {
	result adapter.Result
	err    error
}

func (a *app) waitForNativeExitWithLease(ctx context.Context, c common, runtime adapter.Adapter, ref adapter.SourceRef, execution model.Execution) (model.Execution, adapter.Result, error, *output.Error) {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan nativeWaitOutcome, 1)
	go func() {
		result, err := runtime.Wait(waitCtx, ref)
		done <- nativeWaitOutcome{result: result, err: err}
	}()
	heartbeat := time.NewTicker(nativeRunnerHeartbeatEvery)
	defer heartbeat.Stop()
	for {
		select {
		case outcome := <-done:
			return execution, outcome.result, outcome.err, nil
		case <-heartbeat.C:
			journal, current, problem := a.openExecutionWriteOwned(c, execution.ID)
			if problem != nil {
				return execution, adapter.Result{}, nil, problem
			}
			refreshed, err := refreshRunnerLease(journal, current, a.now().UTC())
			journal.Close()
			if err != nil {
				return execution, adapter.Result{}, nil, mapStoreError("refresh native runner lease while waiting for exit", err)
			}
			execution = refreshed
		}
	}
}

func interruptedRunError(err error, execution model.Execution) *output.Error {
	withExecution := func(problem *output.Error) *output.Error {
		if execution.ID.IsZero() {
			return problem
		}
		return problem.WithDetail("execution_id", execution.ID.String()).WithDetail("state", execution.State)
	}
	if errors.Is(err, context.Canceled) {
		return withExecution(output.Wrap(output.CodeExecutionCancelled, "run interrupted", false, err))
	}
	return withExecution(output.Wrap(output.CodeTimeout, "run deadline elapsed", true, err))
}

func launchInterrupted(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var adapterErr *adapter.AdapterError
	return errors.As(err, &adapterErr) && (adapterErr.Code == adapter.ErrExecutionCancelled || adapterErr.Code == adapter.ErrTimeout)
}

func parseRun(args []string) (runOptions, *output.Error) {
	o := runOptions{}
	delimiter := -1
	for i, arg := range args {
		if arg == "--" {
			delimiter = i
			break
		}
	}
	if delimiter < 0 || delimiter == len(args)-1 {
		return o, output.NewError(output.CodeUsage, "run requires native argv after --", false)
	}
	for i := 0; i < delimiter; i++ {
		switch args[i] {
		case "--adapter":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--adapter requires a value", false)
			}
			i++
			o.adapter = args[i]
		case "--cwd":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--cwd requires a value", false)
			}
			i++
			o.cwd = args[i]
		case "--idempotency-key":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--idempotency-key requires a value", false)
			}
			i++
			o.idempotencyKey = args[i]
		case "--execution-id":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--execution-id requires a value", false)
			}
			i++
			parsed, err := ids.ParseExecutionID(args[i])
			if err != nil {
				return o, output.Wrap(output.CodeUsage, "--execution-id must be a valid exec ID", false, err)
			}
			o.executionID = parsed
		case "--timeout":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--timeout requires a value", false)
			}
			i++
			duration, err := time.ParseDuration(args[i])
			if err != nil || duration <= 0 {
				return o, output.NewError(output.CodeUsage, "--timeout must be a positive duration", false)
			}
			o.timeout = duration
			o.timeoutSet = true
		case "--no-timeout":
			o.timeout = 0
			o.noTimeoutSet = true
		case "--prompt-file":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--prompt-file requires a value", false)
			}
			i++
			o.promptFile = args[i]
		case "--prompt-stdin":
			o.promptStdin = true
		case "--prompt-delivery":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--prompt-delivery requires argv or stdin", false)
			}
			i++
			o.promptDelivery = args[i]
		case "--label":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--label requires a value", false)
			}
			i++
			label := strings.TrimSpace(args[i])
			if !validRunLabel(label) {
				return o, output.NewError(output.CodeUsage, "--label must match ^[a-z][a-z0-9_.-]{0,63}$", false).WithDetail("label", label)
			}
			if containsArg(o.labels, label) {
				return o, output.NewError(output.CodeUsage, "--label values must be unique", false).WithDetail("label", label)
			}
			o.labels = append(o.labels, label)
			if len(o.labels) > 16 {
				return o, output.NewError(output.CodeUsage, "run accepts at most 16 labels", false)
			}
		case "--background":
			o.background = true
		case "--plan":
			o.plan = true
		case "--no-store-result":
			o.noStoreResult = true
		case "--allow-missing-result":
			o.allowMissingResult = true
		case "--allow-unreliable-result":
			o.allowUnreliableResult = true
		case "--issue":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--issue requires a value", false)
			}
			i++
			o.issue = args[i]
		case "--run":
			if i+1 >= delimiter {
				return o, output.NewError(output.CodeUsage, "--run requires a value", false)
			}
			i++
			o.run = args[i]
		default:
			return o, output.NewError(output.CodeUsage, "unknown run flag", false).WithDetail("flag", args[i])
		}
	}
	o.argv = append([]string(nil), args[delimiter+1:]...)
	if o.timeoutSet && o.noTimeoutSet {
		return o, output.NewError(output.CodeUsage, "--timeout and --no-timeout are mutually exclusive", false)
	}
	if o.promptFile != "" && o.promptStdin {
		return o, output.NewError(output.CodeUsage, "--prompt-file and --prompt-stdin are mutually exclusive", false)
	}
	if o.background && o.idempotencyKey != "" {
		return o, output.NewError(output.CodeUsage, "--background cannot use --idempotency-key because startup must return the exact created execution ID", false)
	}
	if o.promptFile == "" && !o.promptStdin && o.promptDelivery != "" {
		return o, output.NewError(output.CodeUsage, "--prompt-delivery requires --prompt-file or --prompt-stdin", false)
	}
	if o.promptDelivery != "" && o.promptDelivery != "argv" && o.promptDelivery != "stdin" {
		return o, output.NewError(output.CodeUsage, "--prompt-delivery must be argv or stdin", false)
	}
	if (o.promptFile != "" || o.promptStdin) && o.promptDelivery == "" {
		o.promptDelivery = "argv"
	}
	if o.adapter == "" {
		o.adapter = inferAdapter(o.argv[0])
	}
	return o, nil
}

func validRunLabel(label string) bool {
	if label == "" || len(label) > 64 || label[0] < 'a' || label[0] > 'z' {
		return false
	}
	for _, ch := range label[1:] {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '.' || ch == '-' {
			continue
		}
		return false
	}
	return true
}

func (a *app) loadPrompt(opts runOptions) (*promptPayload, *output.Error) {
	if opts.promptFile == "" && !opts.promptStdin {
		return nil, nil
	}
	var reader io.Reader
	payload := &promptPayload{Delivery: opts.promptDelivery}
	if opts.promptStdin {
		if a.stdinIsTerminal != nil && a.stdinIsTerminal() {
			return nil, output.NewError(output.CodeUsage, "--prompt-stdin requires piped input", false)
		}
		if a.stdin == nil {
			return nil, output.NewError(output.CodeUsage, "--prompt-stdin has no input stream", false)
		}
		reader = a.stdin
		payload.Source = "stdin"
	} else {
		root := opts.cwd
		if root == "" {
			var err error
			root, err = os.Getwd()
			if err != nil {
				return nil, output.Wrap(output.CodeInternal, "resolve working directory", false, err)
			}
		}
		root, _ = filepath.Abs(root)
		path := opts.promptFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		path, _ = filepath.Abs(filepath.Clean(path))
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, output.NewError(output.CodeAuthorizationDenied, "prompt file must be within the run working root", false).
				WithDetail("path", path).
				WithDetail("root", root).
				WithDetail("recommended_prompt_source", "stdin").
				WithActions(output.NextAction{Label: "Use piped prompt stdin for an external scratch file", Argv: []string{"agentctl", "help", "run"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"select --prompt-stdin and an explicit --prompt-delivery supported by the native argv"}})
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, output.Wrap(output.CodeAuthorizationDenied, "resolve prompt working root", false, err).WithDetail("root", root)
		}
		path = filepath.Join(resolvedRoot, rel)
		file, err := openRegularBelow(resolvedRoot, path)
		if err != nil {
			return nil, output.Wrap(output.CodeAuthorizationDenied, "open prompt file", false, err).WithDetail("path", path)
		}
		defer file.Close()
		reader = file
		payload.Source = "file"
		payload.Path = path
	}
	content, err := io.ReadAll(io.LimitReader(reader, maxPromptBytes+1))
	if err != nil {
		return nil, output.Wrap(output.CodeUsage, "read prompt", false, err)
	}
	if len(content) == 0 {
		return nil, output.NewError(output.CodeUsage, "prompt must not be empty", false)
	}
	if len(content) > maxPromptBytes {
		return nil, output.NewError(output.CodeUsage, "prompt exceeds 8 MiB limit", false).WithDetail("max_bytes", maxPromptBytes)
	}
	payload.Bytes = content
	sum := sha256.Sum256(content)
	payload.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return payload, nil
}

func containsArg(argv []string, value string) bool {
	for _, arg := range argv {
		if arg == value {
			return true
		}
	}
	return false
}

func timeoutDescription(opts runOptions) any {
	if opts.timeout <= 0 {
		return "none"
	}
	return opts.timeout.String()
}

func cursorPlanMode(argv []string) bool {
	for i := 1; i < len(argv); i++ {
		switch argv[i] {
		case "--plan", "--mode=plan":
			return true
		case "--mode":
			if i+1 < len(argv) && strings.EqualFold(argv[i+1], "plan") {
				return true
			}
		}
	}
	return false
}

func inferAdapter(executable string) string {
	name := strings.ToLower(filepath.Base(executable))
	name = strings.TrimSuffix(name, ".exe")
	switch name {
	case "codex":
		return "codex"
	case "cursor", "cursor-agent":
		return "cursor"
	case "claude", "claude-code":
		return "claude"
	case "omp":
		return "omp"
	default:
		return "generic-process"
	}
}

func requireRunCapability(probe adapter.ProbeResult, required adapter.CapabilityName) *output.Error {
	for _, capability := range probe.Capabilities {
		if capability.Name != required {
			continue
		}
		if capability.Status != adapter.CapabilityUnavailable {
			return nil
		}
		problem := output.NewError(output.CodeCapabilityUnavailable, "required adapter capability is unavailable", false).WithDetail("capability", required)
		if capability.Reason != "" {
			problem = problem.WithDetail("reason", capability.Reason)
		}
		if mode, ok := capability.Constraints["required_output_mode"].(string); ok && mode != "" {
			problem = problem.WithDetail("required_output_mode", mode).
				WithDetail("diagnostic_code", "invocation_output_mode_required").
				WithActions(output.NextAction{Label: "Use the adapter's structured output mode", Argv: []string{"agentctl", "help", "run"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"pass the required structured-output flag in the exact native argv"}})
		}
		if requiredArgv, ok := capability.Constraints["required_argv"]; ok {
			problem = problem.WithDetail("required_argv", requiredArgv)
		}
		return problem
	}
	return output.NewError(output.CodeCapabilityUnavailable, "required adapter capability is unavailable", false).WithDetail("capability", required)
}

func ompJSONMode(argv []string) bool {
	mode := ""
	for i := 1; i < len(argv); i++ {
		if argv[i] == "--" {
			break
		}
		if argv[i] == "--mode" && i+1 < len(argv) {
			mode = argv[i+1]
			i++
			continue
		}
		if value, found := strings.CutPrefix(argv[i], "--mode="); found {
			mode = value
		}
	}
	return strings.EqualFold(mode, "json")
}

func (a *app) attachNative(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) != 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl attach <portable-uri>", false)
	}
	uri, err := ids.ParseURI(args[0])
	if err != nil {
		return output.Wrap(output.CodeUsage, "invalid portable URI", false, err)
	}
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	host, err := journal.HostID(ctx)
	if err != nil {
		return mapStoreError("read origin host", err)
	}
	if host != uri.Host {
		return output.NewError(output.CodeDependencyUnavailable, "portable URI names another origin host", true).WithDetail("origin_host_id", uri.Host.String())
	}
	executions, err := journal.ListExecutions(ctx, false)
	if err != nil {
		return mapStoreError("resolve attached source", err)
	}
	wanted := uri.Segments[len(uri.Segments)-1].String()
	var matches []model.Execution
	for _, execution := range executions {
		if uri.Scheme == "agentctl" && execution.ID.String() == wanted {
			matches = append(matches, execution)
			continue
		}
		for _, binding := range execution.SourceBindings {
			if binding.AliasID.String() == wanted {
				matches = append(matches, execution)
				break
			}
		}
	}
	if len(matches) == 1 {
		return writeExecution(renderer, matches[0], "attach")
	}
	if len(matches) > 1 {
		return output.NewError(output.CodeAmbiguousReference, "portable URI matches multiple local executions", false).WithDetail("ref", args[0])
	}
	return output.NewError(output.CodeCapabilityUnavailable, "authority resolver for this portable URI is unavailable", false).WithDetail("scheme", uri.Scheme).WithDetail("ref", args[0])
}

func (a *app) cancelNative(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) != 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl cancel <execution-id>", false)
	}
	id, problem := parseExecutionRef(args[0], c)
	if problem != nil {
		return problem
	}
	journal, problem := a.openWrite(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	execution, err := journal.GetExecution(ctx, id)
	if err != nil {
		return mapStoreError("read execution", err)
	}
	if execution.State.Terminal() {
		return output.NewError(output.CodeInvalidState, "terminal execution cannot be cancelled", false).WithDetail("execution_id", id.String()).WithDetail("state", execution.State)
	}
	runtime, _, problem := a.runtimeAdapter(c, execution.Adapter, bindingOpaque(execution, "issue"), bindingOpaque(execution, "run"))
	if problem != nil {
		return problem
	}
	ref := sourceRef(execution)
	if ref.Empty() {
		return output.NewError(output.CodeCapabilityUnavailable, "execution has no exact native source binding", false).WithDetail("execution_id", id.String())
	}
	if err := runtime.Cancel(ctx, adapter.CancelRequest{Ref: ref, Signal: "term", Grace: 5 * time.Second}); err != nil {
		return mapAdapterError("native cancellation failed", err).WithDetail("execution_id", id.String())
	}
	now := a.now().UTC()
	execution, err = commitCancellation(ctx, journal, execution, now)
	if err != nil {
		return mapStoreError("record cancellation outcome", err)
	}
	return writeExecution(renderer, execution, "cancel")
}

func commitCancellation(ctx context.Context, journal *store.Journal, execution model.Execution, now time.Time) (model.Execution, error) {
	for attempt := 0; attempt < 8; attempt++ {
		current, err := journal.GetExecution(ctx, execution.ID)
		if err != nil {
			return model.Execution{}, err
		}
		if current.State.Terminal() {
			return current, nil
		}
		next := current
		next.State = model.StateCancelled
		next.Liveness = model.LivenessExited
		next.TerminalAt = &now
		next.UpdatedAt = now
		next.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}
		outcome := model.Outcome{SchemaVersion: model.SchemaVersion, ExecutionID: next.ID, Revision: 1, State: model.StateCancelled, Availability: model.OutcomeStored, RecordedAt: now, Source: next.Adapter, ResultRef: fmt.Sprintf("agentctl://%s/%s", next.OriginHostID, next.ID), Failure: &model.OutcomeFailure{Code: "execution_cancelled", Kind: "cancelled", Source: next.Adapter, Retryable: false, Message: "native execution cancelled"}}
		payload := map[string]any{"result_available": true, "outcome_execution_id": next.ID.String(), "availability": outcome.Availability, "failure_code": outcome.Failure.Code}
		event, canonical, err := syntheticEvent(next, model.EventTerminal, model.StateCancelled, payload, "cancel", now)
		if err != nil {
			return model.Execution{}, err
		}
		stored, _, _, _, err := journal.CommitTerminalOutcome(ctx, next, current.Revision, outcome, event, canonical)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		return stored, err
	}
	return model.Execution{}, fmt.Errorf("%w: cancellation could not converge", store.ErrConflict)
}

func (a *app) runtimeAdapter(c common, name, issue, run string) (adapter.Adapter, string, *output.Error) {
	switch name {
	case "codex":
		return adapter.NewCodex(), "", nil
	case "cursor":
		return adapter.NewCursor(), "", nil
	case "claude", "claude-code":
		return adapter.NewClaudeCode(), "", nil
	case "omp":
		return adapter.NewOMP(), "", nil
	case "process", "generic", "generic-process":
		return adapter.NewGenericProcess(), "", nil
	case "multica":
		profileName, profile, problem := a.resolveProfile(c)
		if problem != nil {
			return nil, "", problem
		}
		if profile.Multica == nil {
			return nil, "", output.NewError(output.CodeNotFound, "selected profile has no Multica authority", false).WithDetail("profile", profileName)
		}
		m := profile.Multica
		return adapter.NewMultica(adapter.MulticaConfig{Binary: m.Executable, Profile: m.Profile, Endpoint: m.ServerURL, Workspace: m.WorkspaceID, Issue: issue, Run: run}), profileName, nil
	default:
		return nil, "", output.NewError(output.CodeUsage, "unknown adapter", false).WithDetail("adapter", name)
	}
}
func (a *app) resolveProfile(c common) (string, config.Profile, *output.Error) {
	resolution, err := configResolution(c)
	if err != nil {
		return "", config.Profile{}, output.Wrap(output.CodeNotFound, "load profile config", false, err)
	}
	name, profile, err := resolution.Config.ResolveProfile(c.profile)
	if err != nil {
		return "", config.Profile{}, output.Wrap(output.CodeNotFound, "resolve profile", false, err)
	}
	return name, profile, nil
}
func (a *app) openWrite(c common) (*store.Journal, *output.Error) {
	path, err := a.journalPath(c)
	if err != nil {
		return nil, output.Wrap(output.CodeInternal, "resolve journal path", false, err)
	}
	journal, err := openJournalWithRetry(path, store.Options{})
	if err != nil {
		return nil, mapStoreError("open journal", err)
	}
	return journal, nil
}

func (a *app) openExecutionWrite(ctx context.Context, c common, id ids.ExecutionID) (*store.Journal, model.Execution, *output.Error) {
	journal, problem := a.openWrite(c)
	if problem != nil {
		return nil, model.Execution{}, problem
	}
	execution, err := journal.GetExecution(ctx, id)
	if err != nil {
		journal.Close()
		return nil, model.Execution{}, mapStoreError("read live execution", err)
	}
	return journal, execution, nil
}

// openExecutionWriteOwned waits through transient process-lock contention. The
// caller owns a live native child, so returning ErrBusy would trigger cleanup
// and destroy useful work merely because another agentctl command briefly had
// the journal open.
func (a *app) openExecutionWriteOwned(c common, id ids.ExecutionID) (*store.Journal, model.Execution, *output.Error) {
	path, err := a.journalPath(c)
	if err != nil {
		return nil, model.Execution{}, output.Wrap(output.CodeInternal, "resolve journal path", false, err)
	}
	for {
		journal, openErr := store.Open(path, store.Options{LockTimeout: 2 * time.Second})
		if openErr == nil {
			execution, getErr := journal.GetExecution(context.Background(), id)
			if getErr != nil {
				journal.Close()
				return nil, model.Execution{}, mapStoreError("read live execution", getErr)
			}
			return journal, execution, nil
		}
		if !errors.Is(openErr, store.ErrBusy) {
			return nil, model.Execution{}, mapStoreError("open journal for owned execution", openErr)
		}
		time.Sleep(time.Duration(100+time.Now().UnixNano()%250) * time.Millisecond)
	}
}

func capabilitySnapshot(probe adapter.ProbeResult) model.CapabilitySnapshot {
	backend := probe.BackendVersion
	items := make([]model.CapabilityItem, 0, len(probe.Capabilities))
	for _, item := range probe.Capabilities {
		var reason *string
		if item.Reason != "" {
			value := item.Reason
			reason = &value
		}
		items = append(items, model.CapabilityItem{Name: string(item.Name), Status: model.CapabilityStatus(item.Status), Source: item.Source, SemanticsVersion: item.SemanticsVersion, Constraints: item.Constraints, Reason: reason})
	}
	return model.CapabilitySnapshot{NegotiatedAt: probe.ProbedAt, AdapterVersion: probe.AdapterVersion, BackendVersion: &backend, Items: items}
}
func contextInput(c common) *adapter.ContextInput {
	if c.contextFile == "" {
		return nil
	}
	return &adapter.ContextInput{Path: c.contextFile, Required: false}
}
func mutationDigest(name, cwd string, argv, labels []string, noStoreResult bool, prompt *promptPayload) (string, error) {
	promptDigest, promptDelivery := "", ""
	if prompt != nil {
		promptDigest, promptDelivery = prompt.Digest, prompt.Delivery
	}
	canonical, err := callback.CanonicalJSON(map[string]any{"adapter": name, "cwd": cwd, "argv": argv, "labels": labels, "no_store_result": noStoreResult, "prompt_sha256": promptDigest, "prompt_delivery": promptDelivery})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func applySession(journal *store.Journal, execution model.Execution, session adapter.Session, now time.Time) (model.Execution, error) {
	state := toModelState(session.State)
	liveness := toModelLiveness(session.Liveness)
	// A fast child may already be terminal when launch returns. Terminal state
	// is committed only with its outcome and terminal event below.
	if state.Terminal() {
		state = model.StateRunning
		liveness = model.LivenessAlive
	}
	execution.State = state
	execution.Liveness = liveness
	execution.UpdatedAt = now
	observedAt := session.Observation.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	leaseSeconds := nativeRunnerLeaseSeconds
	execution.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: observedAt, FreshForSeconds: &leaseSeconds}
	started := session.StartedAt
	if !started.IsZero() {
		execution.StartedAt = &started
	}
	binding := session.Binding
	if binding.Fingerprint != "" {
		alias, err := ids.New(ids.TypeSource)
		if err != nil {
			return execution, err
		}
		opaque := binding.OpaqueID
		execution.SourceBindings = []model.SourceBinding{{Kind: binding.Kind, AliasID: alias, Fingerprint: binding.Fingerprint, OpaqueID: &opaque}}
	}
	return journal.UpdateExecution(context.Background(), execution, execution.Revision)
}

func refreshRunnerLease(journal *store.Journal, execution model.Execution, now time.Time) (model.Execution, error) {
	if execution.State.Terminal() {
		return execution, nil
	}
	leaseSeconds := nativeRunnerLeaseSeconds
	execution.Liveness = runnerLeaseLiveness(execution.State)
	execution.UpdatedAt = now
	execution.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now, FreshForSeconds: &leaseSeconds}
	return journal.UpdateExecution(context.Background(), execution, execution.Revision)
}

func runnerLeaseLiveness(state model.State) model.Liveness {
	switch state {
	case model.StateWaiting, model.StateAttention:
		return model.LivenessBlocked
	default:
		return model.LivenessAlive
	}
}
func finalizeResult(ctx context.Context, journal *store.Journal, execution model.Execution, result adapter.Result, now time.Time, noStoreResult bool) (model.Execution, error) {
	execution.State = toModelState(result.State)
	execution.Liveness = model.LivenessExited
	execution.TerminalAt = &now
	execution.UpdatedAt = now
	execution.Observation.ObservedAt = now
	execution.Observation.Integrity = model.IntegrityVerified
	if sourceState, ok := result.Data["terminal_source_state"].(string); ok && strings.TrimSpace(sourceState) != "" {
		sourceState = strings.TrimSpace(sourceState)
		execution.SourceState = &sourceState
	}
	outcome := buildOutcome(execution, result, now, noStoreResult)
	payload := map[string]any{"result_available": outcome.Availability == model.OutcomeStored, "outcome_execution_id": execution.ID.String(), "availability": outcome.Availability}
	if outcome.Content != nil {
		payload["content_available"] = true
		payload["content_sha256"] = outcome.Content.SHA256
	}
	if outcome.Failure != nil {
		payload["failure_code"] = outcome.Failure.Code
	}
	for _, key := range []string{"diagnostic_code", "parse_diagnostic_code", "result_content_source", "terminal_source_state"} {
		if value, ok := result.Data[key].(string); ok && strings.TrimSpace(value) != "" {
			payload[key] = value
		}
	}
	event, canonical, err := syntheticEvent(execution, model.EventTerminal, execution.State, payload, "result", now)
	if err != nil {
		return execution, err
	}
	updated, _, _, _, err := journal.CommitTerminalOutcome(ctx, execution, execution.Revision, outcome, event, canonical)
	return updated, err
}

func finalizeResultConverging(ctx context.Context, journal *store.Journal, execution model.Execution, result adapter.Result, now time.Time, noStoreResult bool) (model.Execution, error) {
	updated, err := finalizeResult(ctx, journal, execution, result, now, noStoreResult)
	if !errors.Is(err, store.ErrTerminalConflict) {
		return updated, err
	}
	latest, readErr := journal.GetExecution(ctx, execution.ID)
	if readErr != nil {
		return model.Execution{}, readErr
	}
	if latest.State.Terminal() {
		return latest, nil
	}
	return model.Execution{}, err
}

func buildOutcome(execution model.Execution, result adapter.Result, now time.Time, noStoreResult bool) model.Outcome {
	outcome := model.Outcome{SchemaVersion: model.SchemaVersion, ExecutionID: execution.ID, Revision: 1, State: execution.State, RecordedAt: now, Source: execution.Adapter, ResultRef: fmt.Sprintf("agentctl://%s/%s", execution.OriginHostID, execution.ID), NativeExitCode: result.ExitCode}
	if noStoreResult {
		outcome.Availability = model.OutcomeOmittedByPolicy
	} else {
		content := strings.TrimSpace(result.Content)
		if content != "" {
			bounded, truncated := boundedOutcomeText(content, model.OutcomeInlineLimit)
			preview, _ := boundedOutcomeText(bounded, model.OutcomePreviewLimit)
			contentSource, _ := result.Data["result_content_source"].(string)
			item := &model.OutcomeContent{MediaType: "text/plain", Source: strings.TrimSpace(contentSource), Text: bounded, Preview: preview, Bytes: len(content), Truncated: truncated || result.ContentTruncated}
			if !item.Truncated {
				digest := sha256.Sum256([]byte(content))
				item.SHA256 = "sha256:" + hex.EncodeToString(digest[:])
			}
			outcome.Content = item
		}
		if strings.TrimSpace(result.Error) != "" || execution.State == model.StateFailed {
			message, _ := boundedOutcomeText(firstNonEmptyString(result.Error, "native execution failed"), model.OutcomeFailureLimit)
			diagnosticCode, _ := result.Data["diagnostic_code"].(string)
			code, kind, retryable := classifyOutcomeFailure(message, diagnosticCode)
			outcome.Failure = &model.OutcomeFailure{Code: code, Kind: kind, Source: execution.Adapter, Retryable: retryable, Message: message}
		}
		if outcome.Content != nil || outcome.Failure != nil {
			outcome.Availability = model.OutcomeStored
		} else {
			outcome.Availability = model.OutcomeUnavailableAtSource
		}
	}
	return outcome
}

func boundedOutcomeText(value string, max int) (string, bool) {
	if len(value) <= max {
		return value, false
	}
	value = value[:max]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func classifyOutcomeFailure(message, diagnosticCode string) (string, string, bool) {
	if diagnosticCode == "result_extraction_failed" {
		return "result_extraction_failed", "observation", false
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "workspace") && strings.Contains(lower, "trust"):
		return "workspace_trust_required", "permission", true
	case strings.Contains(lower, "auth") || strings.Contains(lower, "login") || strings.Contains(lower, "sign in"):
		return "authentication_required", "authentication", true
	case strings.Contains(lower, "approval") || strings.Contains(lower, "permission"):
		return "approval_required", "approval", true
	case strings.Contains(lower, "quota") || strings.Contains(lower, "rate limit"):
		return "quota_exceeded", "quota", true
	default:
		return "native_execution_failed", "unknown", false
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func syntheticEvent(execution model.Execution, kind model.EventKind, state model.State, payload map[string]any, source string, now time.Time) (model.Event, []byte, error) {
	projection := map[string]any{"authority_scope": execution.Authority, "execution_fingerprint": execution.ID.String(), "kind": kind, "source": source, "state": state}
	key, canonical, err := callback.SemanticDedupeKey(execution.Adapter, 1, projection)
	if err != nil {
		return model.Event{}, nil, err
	}
	stateValue := state
	event := model.Event{ExecutionID: execution.ID, OriginHostID: execution.OriginHostID, Ordering: model.OrderingObservation, Kind: kind, State: &stateValue, Authority: execution.Authority, Adapter: execution.Adapter, ObservedAt: now, DedupeKey: key, DedupeVersion: 1, Payload: payload}
	return event, canonical, nil
}
func appendSynthetic(ctx context.Context, journal *store.Journal, execution model.Execution, kind model.EventKind, state model.State, payload map[string]any, source string) error {
	projection := map[string]any{"authority_scope": execution.Authority, "execution_fingerprint": execution.ID.String(), "kind": kind, "source": source, "state": state}
	key, canonical, err := callback.SemanticDedupeKey(execution.Adapter, 1, projection)
	if err != nil {
		return err
	}
	var statePtr *model.State
	if kind == model.EventTerminal || state != "" {
		value := state
		statePtr = &value
	}
	event := model.Event{ExecutionID: execution.ID, OriginHostID: execution.OriginHostID, Ordering: model.OrderingObservation, Kind: kind, State: statePtr, Authority: execution.Authority, Adapter: execution.Adapter, ObservedAt: time.Now().UTC(), DedupeKey: key, DedupeVersion: 1, Payload: payload}
	_, _, err = journal.AppendEvent(ctx, event, canonical)
	return err
}
func appendNativeEvent(ctx context.Context, journal *store.Journal, execution model.Execution, native adapter.Event) (model.Execution, error) {
	kind := model.EventKind(native.Kind)
	switch kind {
	case model.EventStarted, model.EventProgress, model.EventAttention, model.EventArtifact, model.EventHealth, model.EventTerminal, model.EventPromoted, model.EventSuperseded:
	default:
		kind = model.EventProgress
	}
	state := toModelState(native.State)
	var statePtr *model.State
	if state != "" {
		statePtr = &state
	}
	ordering := model.Ordering(native.Ordering)
	if ordering == "" {
		ordering = model.OrderingObservation
	}
	var position *model.SourcePosition
	if ordering == model.OrderingSource || ordering == model.OrderingReconciled {
		value := native.Cursor
		if value == "" {
			value = strconv.FormatInt(native.Sequence, 10)
		}
		position = &model.SourcePosition{Kind: "native_sequence", Value: value}
	}
	payload := safeNativePayload(native.Payload)
	projection := map[string]any{"authority_scope": execution.Authority, "source_fingerprint": sourceRef(execution).Fingerprint, "kind": kind, "source_state": native.SourceState, "source_position": native.Cursor, "state": state, "payload": payload}
	key, canonical, err := callback.SemanticDedupeKey(execution.Adapter, 1, projection)
	if err != nil {
		return model.Execution{}, err
	}
	observedAt := native.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	event := model.Event{ExecutionID: execution.ID, OriginHostID: execution.OriginHostID, Ordering: ordering, SourcePosition: position, Kind: kind, State: statePtr, SourceState: stringPointer(native.SourceState), Authority: execution.Authority, Adapter: execution.Adapter, OccurredAt: native.OccurredAt, ObservedAt: native.ObservedAt, DedupeKey: key, DedupeVersion: 1, Payload: payload}
	event.ObservedAt = observedAt
	next := execution
	if state != "" && !state.Terminal() {
		next.State = state
	}
	if native.SourceState != "" {
		next.SourceState = stringPointer(native.SourceState)
	}
	switch next.State {
	case model.StateWaiting, model.StateAttention:
		next.Liveness = model.LivenessBlocked
	case model.StateStarting, model.StateRunning:
		next.Liveness = model.LivenessAlive
	}
	freshFor := int(nativeEventFreshness / time.Second)
	next.UpdatedAt = observedAt
	next.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: observedAt, FreshForSeconds: &freshFor}
	stored, _, _, err := journal.CommitObservedEvent(ctx, next, execution.Revision, event, canonical)
	return stored, err
}
func safeNativePayload(input map[string]any) map[string]any {
	output := map[string]any{}
	for _, key := range []string{"family", "type", "is_error", "status", "state", "subtype", "usage", "artifact_ref", "attention_kind", "diagnostic_code", "progress_phase"} {
		if value, ok := input[key]; ok {
			output[key] = value
		}
	}
	return output
}
func sourceRef(execution model.Execution) adapter.SourceRef {
	if len(execution.SourceBindings) == 0 {
		return adapter.SourceRef{}
	}
	binding := execution.SourceBindings[0]
	ref := adapter.SourceRef{Adapter: execution.Adapter, Kind: binding.Kind, Fingerprint: binding.Fingerprint}
	if binding.OpaqueID != nil {
		ref.OpaqueID = *binding.OpaqueID
	}
	if pid, err := strconv.Atoi(ref.OpaqueID); err == nil {
		ref.PID = pid
	}
	for _, item := range execution.SourceBindings {
		if item.OpaqueID == nil {
			continue
		}
		switch {
		case strings.Contains(item.Kind, "project"):
			ref.Workspace = *item.OpaqueID
		case strings.Contains(item.Kind, "issue"):
			ref.Issue = *item.OpaqueID
		case strings.Contains(item.Kind, "run"):
			ref.Run = *item.OpaqueID
		}
	}
	return ref
}
func bindingOpaque(execution model.Execution, kind string) string {
	for _, binding := range execution.SourceBindings {
		if strings.Contains(binding.Kind, kind) && binding.OpaqueID != nil {
			return *binding.OpaqueID
		}
	}
	return ""
}
func terminalAdapterState(state adapter.State) bool {
	return state == adapter.StateCompleted || state == adapter.StateFailed || state == adapter.StateCancelled || state == adapter.StateOrphaned
}
func toModelState(state adapter.State) model.State          { return model.State(state) }
func toModelLiveness(value adapter.Liveness) model.Liveness { return model.Liveness(value) }
func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
func mapAdapterError(message string, err error) *output.Error {
	var adapterErr *adapter.AdapterError
	if errors.As(err, &adapterErr) {
		codes := map[adapter.ErrorCode]output.Code{adapter.ErrCapabilityUnavailable: output.CodeCapabilityUnavailable, adapter.ErrDependencyUnavailable: output.CodeDependencyUnavailable, adapter.ErrAuthenticationRequired: output.CodeAuthenticationRequired, adapter.ErrNotFound: output.CodeNotFound, adapter.ErrTimeout: output.CodeTimeout, adapter.ErrExecutionFailed: output.CodeExecutionFailed, adapter.ErrExecutionCancelled: output.CodeExecutionCancelled, adapter.ErrExecutionUnknown: output.CodeExecutionUnknown, adapter.ErrUsage: output.CodeUsage, adapter.ErrInternal: output.CodeInternal}
		code := codes[adapterErr.Code]
		if code == "" {
			code = output.CodeInternal
		}
		result := output.Wrap(code, message, adapterErr.Retryable, err)
		result.WithDetail("reason", adapterErr.Message)
		for key, value := range adapterErr.Details {
			result.WithDetail(key, value)
		}
		return result
	}
	return output.Wrap(output.CodeInternal, message, false, err)
}

var _ = fmt.Sprintf
