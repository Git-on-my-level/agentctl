package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	executionID                              ids.ExecutionID
	plan                                     bool
	noStoreResult                            bool
	allowMissingResult                       bool
	timeout                                  time.Duration
	argv                                     []string
}

const (
	nativeRunnerLeaseSeconds   = 5
	nativeRunnerHeartbeatEvery = time.Second
	defaultRunTimeout          = 30 * time.Minute
)

func (a *app) runNative(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	opts, problem := parseRun(args)
	if problem != nil {
		return problem
	}
	runtime, profileName, problem := a.runtimeAdapter(c, opts.adapter, opts.issue, opts.run)
	if problem != nil {
		return problem
	}
	operationCtx := ctx
	var operationCancel context.CancelFunc
	if opts.timeout > 0 {
		operationCtx, operationCancel = context.WithTimeout(ctx, opts.timeout)
		defer operationCancel()
	}
	probe, err := runtime.Probe(operationCtx, adapter.ProbeRequest{Executable: opts.argv[0], Profile: profileName, Timeout: 5 * time.Second, Fresh: true})
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
		result := map[string]any{"adapter": runtime.Name(), "executable": opts.argv[0], "argument_count": len(opts.argv) - 1, "profile": profileName, "side_effect_class": output.ExternalSideEffect, "probe": probe, "writes_local_state": true, "stores_result": !opts.noStoreResult}
		if !opts.executionID.IsZero() {
			result["execution_id"] = opts.executionID.String()
		}
		if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "plan", Fields: []output.Field{{Name: "command", Value: "run"}, {Name: "adapter", Value: runtime.Name()}, {Name: "executable", Value: opts.argv[0]}, {Name: "arguments", Value: len(opts.argv) - 1}, {Name: "side_effect_class", Value: output.ExternalSideEffect}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	}
	journal, problem := a.openWrite(c)
	if problem != nil {
		return problem
	}
	now := a.now().UTC()
	fresh := int(probe.FreshFor / time.Second)
	execution := model.Execution{ID: opts.executionID, Authority: model.AuthorityNative, Adapter: runtime.Name(), Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateStarting, Liveness: model.LivenessUnknown, SourceBindings: []model.SourceBinding{}, Capabilities: capabilitySnapshot(probe), Supersedes: []ids.ExecutionID{}, Observation: model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityUnknown, ObservedAt: now, FreshForSeconds: &fresh}}
	mutation := contracts.MutationKey{}
	if opts.idempotencyKey != "" {
		digest, dErr := mutationDigest(runtime.Name(), opts.cwd, opts.argv, opts.noStoreResult)
		if dErr != nil {
			journal.Close()
			return output.Wrap(output.CodeInternal, "canonicalize run idempotency inputs", false, dErr)
		}
		mutation = contracts.MutationKey{Scope: "execution:run", Key: opts.idempotencyKey, InputDigest: digest}
	}
	execution, reused, err := journal.CreateExecution(ctx, execution, mutation)
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
	// must be able to observe the execution while it is running.
	journal.Close()
	launchCtx := operationCtx
	launch, err := runtime.Launch(launchCtx, adapter.LaunchRequest{Argv: opts.argv, Cwd: opts.cwd, Context: contextInput(c), DiscoveryWindow: 250 * time.Millisecond, StartOnly: true})
	if err != nil {
		writeJournal, current, openProblem := a.openExecutionWrite(context.Background(), c, execution.ID)
		if openProblem != nil {
			return openProblem
		}
		failed, finalizeErr := finalizeResult(context.Background(), writeJournal, current, adapter.Result{Success: false, State: adapter.StateFailed, Error: err.Error()}, a.now().UTC(), opts.noStoreResult)
		writeJournal.Close()
		if finalizeErr != nil {
			return mapStoreError("record launch failure outcome", finalizeErr)
		}
		return mapAdapterError("launch failed", err).WithDetail("execution_id", failed.ID.String())
	}
	writeJournal, current, openProblem := a.openExecutionWrite(context.Background(), c, execution.ID)
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
			writeJournal, current, openProblem = a.openExecutionWrite(context.Background(), c, execution.ID)
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
			writeJournal, current, openProblem = a.openExecutionWrite(context.Background(), c, execution.ID)
			if openProblem != nil {
				return openProblem
			}
			execution = current
			for _, nativeEvent := range nativeEvents {
				if nativeEvent.Cursor != "" {
					cursor = nativeEvent.Cursor
				}
				if nativeEvent.Kind == "terminal" {
					continue
				}
				if err := appendNativeEvent(context.Background(), writeJournal, execution, nativeEvent); err != nil && !errors.Is(err, store.ErrTerminalConflict) {
					writeJournal.Close()
					return mapStoreError("record native event", err)
				}
			}
			writeJournal.Close()
		}
		result, resultErr := runtime.Result(launchCtx, adapter.ResultRequest{Ref: launch.Session.Ref})
		if resultErr == nil && terminalAdapterState(result.State) {
			// The structured stream may announce terminal state just before the
			// process exits. Reap the child before returning so a foreground run
			// never leaves an orphan behind.
			if waited, waitErr := runtime.Wait(launchCtx, launch.Session.Ref); waitErr == nil {
				result = waited
			} else if !errors.Is(waitErr, context.Canceled) && !errors.Is(waitErr, context.DeadlineExceeded) {
				return mapAdapterError("wait for native process exit", waitErr).WithDetail("execution_id", execution.ID.String())
			}
			writeJournal, current, openProblem = a.openExecutionWrite(context.Background(), c, execution.ID)
			if openProblem != nil {
				return openProblem
			}
			execution, err = finalizeResult(context.Background(), writeJournal, current, result, a.now().UTC(), opts.noStoreResult)
			writeJournal.Close()
			if err != nil {
				return mapStoreError("record terminal result", err)
			}
			if launchCtx.Err() != nil {
				return interruptedRunError(launchCtx.Err(), execution)
			}
			return writeExecution(renderer, execution, "run")
		}
		select {
		case <-launchCtx.Done():
			last, _ := runtime.Result(context.Background(), adapter.ResultRequest{Ref: launch.Session.Ref})
			if !terminalAdapterState(last.State) {
				waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
				waited, waitErr := runtime.Wait(waitCtx, launch.Session.Ref)
				waitCancel()
				if waitErr == nil && terminalAdapterState(waited.State) {
					last = waited
				} else {
					last = adapter.Result{Success: false, State: adapter.StateFailed, Error: "agentctl run deadline elapsed", SessionRef: launch.Session.Ref}
				}
			}
			writeJournal, current, openProblem := a.openExecutionWrite(context.Background(), c, execution.ID)
			if openProblem != nil {
				return openProblem
			}
			execution, err = finalizeResult(context.Background(), writeJournal, current, last, a.now().UTC(), opts.noStoreResult)
			writeJournal.Close()
			if err != nil {
				return mapStoreError("record timed out execution", err)
			}
			return interruptedRunError(launchCtx.Err(), execution)
		case <-ticker.C:
		}
	}
}

func interruptedRunError(err error, execution model.Execution) *output.Error {
	if errors.Is(err, context.Canceled) {
		return output.Wrap(output.CodeExecutionCancelled, "run interrupted", false, err).WithDetail("execution_id", execution.ID.String()).WithDetail("state", execution.State)
	}
	return output.Wrap(output.CodeTimeout, "run deadline elapsed", true, err).WithDetail("execution_id", execution.ID.String()).WithDetail("state", execution.State)
}

func parseRun(args []string) (runOptions, *output.Error) {
	o := runOptions{timeout: defaultRunTimeout}
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
		case "--no-timeout":
			o.timeout = 0
		case "--plan":
			o.plan = true
		case "--no-store-result":
			o.noStoreResult = true
		case "--allow-missing-result":
			o.allowMissingResult = true
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
	if o.adapter == "" {
		o.adapter = inferAdapter(o.argv[0])
	}
	return o, nil
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
		break
	}
	return output.NewError(output.CodeCapabilityUnavailable, "required adapter capability is unavailable", false).WithDetail("capability", required)
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
	execution.State = model.StateCancelled
	execution.Liveness = model.LivenessExited
	execution.TerminalAt = &now
	execution.UpdatedAt = now
	execution.Observation.ObservedAt = now
	execution.Observation.Integrity = model.IntegrityVerified
	execution, err = journal.UpdateExecution(ctx, execution, execution.Revision)
	if err != nil {
		return mapStoreError("record cancellation", err)
	}
	if err := appendSynthetic(ctx, journal, execution, model.EventTerminal, model.StateCancelled, map[string]any{"cancelled": true}, "cancel"); err != nil {
		return mapStoreError("record cancellation event", err)
	}
	return writeExecution(renderer, execution, "cancel")
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
	path := c.configPath
	var err error
	if path == "" {
		path, err = config.DefaultPath()
		if err != nil {
			return "", config.Profile{}, output.Wrap(output.CodeInternal, "resolve config path", false, err)
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", config.Profile{}, output.Wrap(output.CodeNotFound, "load profile config", false, err)
	}
	name, profile, err := cfg.ResolveProfile(c.profile)
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
	journal, err := store.Open(path, store.Options{})
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
func mutationDigest(name, cwd string, argv []string, noStoreResult bool) (string, error) {
	canonical, err := callback.CanonicalJSON(map[string]any{"adapter": name, "cwd": cwd, "argv": argv, "no_store_result": noStoreResult})
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
	execution.Liveness = model.LivenessAlive
	execution.UpdatedAt = now
	execution.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now, FreshForSeconds: &leaseSeconds}
	return journal.UpdateExecution(context.Background(), execution, execution.Revision)
}
func finalizeResult(ctx context.Context, journal *store.Journal, execution model.Execution, result adapter.Result, now time.Time, noStoreResult bool) (model.Execution, error) {
	execution.State = toModelState(result.State)
	execution.Liveness = model.LivenessExited
	execution.TerminalAt = &now
	execution.UpdatedAt = now
	execution.Observation.ObservedAt = now
	execution.Observation.Integrity = model.IntegrityVerified
	outcome := buildOutcome(execution, result, now, noStoreResult)
	payload := map[string]any{"result_available": outcome.Availability == model.OutcomeStored, "outcome_execution_id": execution.ID.String(), "availability": outcome.Availability}
	if outcome.Content != nil {
		payload["content_available"] = true
		payload["content_sha256"] = outcome.Content.SHA256
	}
	if outcome.Failure != nil {
		payload["failure_code"] = outcome.Failure.Code
	}
	event, canonical, err := syntheticEvent(execution, model.EventTerminal, execution.State, payload, "result", now)
	if err != nil {
		return execution, err
	}
	updated, _, _, _, err := journal.CommitTerminalOutcome(ctx, execution, execution.Revision, outcome, event, canonical)
	return updated, err
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
			item := &model.OutcomeContent{MediaType: "text/plain", Text: bounded, Preview: preview, Bytes: len(content), Truncated: truncated || result.ContentTruncated}
			if !item.Truncated {
				digest := sha256.Sum256([]byte(content))
				item.SHA256 = "sha256:" + hex.EncodeToString(digest[:])
			}
			outcome.Content = item
		}
		if strings.TrimSpace(result.Error) != "" || execution.State == model.StateFailed {
			message, _ := boundedOutcomeText(firstNonEmptyString(result.Error, "native execution failed"), model.OutcomeFailureLimit)
			code, kind, retryable := classifyOutcomeFailure(message)
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

func classifyOutcomeFailure(message string) (string, string, bool) {
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
func appendNativeEvent(ctx context.Context, journal *store.Journal, execution model.Execution, native adapter.Event) error {
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
		return err
	}
	event := model.Event{ExecutionID: execution.ID, OriginHostID: execution.OriginHostID, Ordering: ordering, SourcePosition: position, Kind: kind, State: statePtr, SourceState: stringPointer(native.SourceState), Authority: execution.Authority, Adapter: execution.Adapter, OccurredAt: native.OccurredAt, ObservedAt: native.ObservedAt, DedupeKey: key, DedupeVersion: 1, Payload: payload}
	_, _, err = journal.AppendEvent(ctx, event, canonical)
	return err
}
func safeNativePayload(input map[string]any) map[string]any {
	output := map[string]any{}
	for _, key := range []string{"family", "type", "is_error", "status", "state", "subtype", "usage", "artifact_ref", "attention_kind", "diagnostic_code"} {
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
		for key, value := range adapterErr.Details {
			result.WithDetail(key, value)
		}
		return result
	}
	return output.Wrap(output.CodeInternal, message, false, err)
}

var _ = fmt.Sprintf
