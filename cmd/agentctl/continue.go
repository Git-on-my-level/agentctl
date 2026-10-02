package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

// A follow-up turn sends the next instruction to the native session of a
// finished delegated execution. The native CLI still owns the conversation:
// agentctl relaunches it with its own resume flag and the frozen launch
// recipe, as a new execution that supersedes the one it continues. Each turn
// therefore keeps its own result, events, and collection state.

const continueUsage = "usage: agentctl continue <execution-id> --request-key key (--prompt-file path|--prompt-stdin) [--plan] [--wait [--content] [--require-result-source source] [--min-result-bytes n]] [--timeout duration] [--label name ...]"

// admissionRefusedSource marks an execution journaled only to close a
// check-then-launch race. Nothing native was started, so its request key
// must remain reusable.
const admissionRefusedSource = "admission_refused"

func refusedAdmission(execution model.Execution) bool {
	return execution.State == model.StateCancelled && execution.SourceState != nil && *execution.SourceState == admissionRefusedSource
}

func parseContinue(args []string) (string, string, delegateOptions, *output.Error) {
	opts := delegateOptions{authority: "native"}
	ref, key := "", ""
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if strings.HasPrefix(flag, "--") {
			if seen[flag] && flag != "--label" {
				return "", "", opts, delegateError(output.CodeUsage, "continue_duplicate_flag", "continue flag supplied more than once").WithDetail("flag", flag)
			}
			seen[flag] = true
		}
		switch flag {
		case "--plan":
			opts.plan = true
		case "--wait":
			opts.wait = true
		case "--content":
			opts.contentOnly = true
		case "--prompt-stdin":
			opts.promptStdin = true
		case "--request-key", "--prompt-file", "--timeout", "--label", "--require-result-source", "--min-result-bytes":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return "", "", opts, delegateError(output.CodeUsage, "continue_missing_flag_value", "continue flag requires a nonempty value").WithDetail("flag", flag)
			}
			i++
			value := args[i]
			switch flag {
			case "--request-key":
				if len(value) > 256 || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
					return "", "", opts, delegateError(output.CodeUsage, "continue_invalid_request_key", "request key must be at most 256 printable bytes")
				}
				key = value
			case "--prompt-file":
				opts.promptFile = value
			case "--timeout":
				v, err := time.ParseDuration(value)
				if err != nil || v <= 0 {
					return "", "", opts, delegateError(output.CodeUsage, "continue_invalid_timeout", "timeout must be a positive Go duration")
				}
				opts.timeout = v
			case "--label":
				if !validRunLabel(value) || containsArg(opts.labels, value) || len(opts.labels) >= 16 {
					return "", "", opts, delegateError(output.CodeUsage, "continue_invalid_label", "continue requires at most 16 distinct exact labels")
				}
				opts.labels = append(opts.labels, value)
			case "--require-result-source":
				opts.requireSource = value
			case "--min-result-bytes":
				n, err := strconv.Atoi(value)
				if err != nil || n < 0 || n > 1<<20 {
					return "", "", opts, delegateError(output.CodeUsage, "continue_invalid_result_bound", "minimum result bytes must be between 0 and 1048576")
				}
				opts.minBytes = n
			}
		default:
			if strings.HasPrefix(flag, "-") || ref != "" {
				return "", "", opts, output.NewError(output.CodeUsage, continueUsage, false).WithDetail("argument", flag)
			}
			ref = flag
		}
	}
	if ref == "" || key == "" || boolCount(opts.promptFile != "", opts.promptStdin) != 1 {
		return "", "", opts, output.NewError(output.CodeUsage, continueUsage, false)
	}
	if (opts.contentOnly || opts.requireSource != "" || opts.minBytes > 0) && !opts.wait {
		return "", "", opts, delegateError(output.CodeUsage, "continue_wait_required", "content and result assertions require --wait")
	}
	if opts.plan && (opts.contentOnly || opts.wait) {
		return "", "", opts, delegateError(output.CodeUsage, "continue_plan_content_conflict", "--plan cannot be combined with --wait or result collection options")
	}
	sort.Strings(opts.labels)
	return ref, key, opts, nil
}

func continueError(code output.Code, diagnostic, message string, source model.Execution) *output.Error {
	return output.NewError(code, message, false).WithDetail("diagnostic_code", diagnostic).WithDetail("execution_id", source.ID.String()).WithDetail("state", source.State)
}

// continuationSuccessors returns the turns already launched from source.
func continuationSuccessors(all []model.Execution, source ids.ExecutionID) []model.Execution {
	var out []model.Execution
	for _, execution := range all {
		for _, id := range execution.Supersedes {
			if id == source && execution.Acquisition == model.AcquisitionLaunched {
				out = append(out, execution)
			}
		}
	}
	return out
}

func (a *app) continueCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	ref, requestKey, opts, problem := parseContinue(args)
	if problem != nil {
		return problem
	}
	if c.contextFile != "" {
		return delegateError(output.CodeCapabilityUnavailable, "continue_context_unpinned", "a follow-up turn does not pin external context handles; supply bounded context in the prompt")
	}
	id, problem := parseExecutionRef(ref, c)
	if problem != nil {
		return problem
	}
	prompt, problem := a.loadPromptForCommand("continue", runOptions{promptFile: opts.promptFile, promptStdin: opts.promptStdin, promptDelivery: "argv"})
	if problem != nil {
		return problem
	}
	if prompt == nil || len(bytes.TrimSpace(prompt.Bytes)) == 0 {
		return delegateError(output.CodeUsage, "continue_empty_prompt", "continue requires a nonempty prompt")
	}
	profileName, _, problem := a.resolveProfile(c)
	if problem != nil {
		return problem
	}
	inputDigest, err := digestJSON(map[string]any{"continues": id.String(), "prompt_sha256": prompt.Digest, "timeout": opts.timeout.String(), "labels": opts.labels})
	if err != nil {
		return output.Wrap(output.CodeInternal, "digest continue inputs", false, err)
	}
	key, err := digestJSON(map[string]string{"profile": profileName, "key": requestKey})
	if err != nil {
		return output.Wrap(output.CodeInternal, "digest continue key", false, err)
	}
	mutation := contracts.MutationKey{Scope: "execution:continue", Key: key, InputDigest: inputDigest}
	// A retry of the same key and inputs recovers the turn it already
	// started, whatever has happened to the session since.
	previous, found, problem := a.findDelegation(ctx, c, mutation)
	if problem != nil {
		return problem
	}
	if found && !refusedAdmission(previous) {
		if previous.Delegation == nil {
			return delegateError(output.CodeInternal, "delegate_binding_missing", "continued execution is missing its resolution binding").WithDetail("execution_id", previous.ID.String())
		}
		if opts.plan {
			return writeContinuePlan(renderer, id, *previous.Delegation, nil, previous.ID.String())
		}
		return a.collectContinuation(ctx, renderer, c, id, previous, opts, true)
	}

	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	source, err := journal.GetExecution(ctx, id)
	var all []model.Execution
	if err == nil {
		all, err = journal.ListExecutions(ctx, false)
	}
	journal.Close()
	if err != nil {
		return mapStoreError("read execution to continue", err)
	}
	if source.Authority != model.AuthorityNative {
		return continueError(output.CodeCapabilityUnavailable, "continue_authority_unavailable", "only native executions can be continued; Multica owns issue and run continuation", source)
	}
	if !source.State.Terminal() {
		return continueError(output.CodeInvalidState, "continue_source_running", "the execution is still running; wait for it, or redirect it with steer", source).
			WithActions(output.NextAction{Label: "Wait for the running turn", Argv: []string{"agentctl", "await", id.String()}, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}},
				output.NextAction{Label: "Inspect steering", Argv: []string{"agentctl", "help", "steer"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	if source.State != model.StateCompleted {
		return continueError(output.CodeInvalidState, "continue_source_not_completed", "only a completed turn can be continued; a failed or cancelled turn may not have kept its context", source)
	}
	if source.Delegation == nil || source.Delegation.NativePlan == nil {
		return continueError(output.CodeCapabilityUnavailable, "continue_launch_plan_missing", "only delegated executions record the launch plan a follow-up turn needs; start the work with delegate", source).
			WithActions(output.NextAction{Label: "Discover delegate", Argv: []string{"agentctl", "help", "delegate"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	for _, successor := range continuationSuccessors(all, id) {
		if !successor.State.Terminal() {
			return continueError(output.CodeConflict, "continue_in_progress", "a follow-up turn on this session is already running", source).WithDetail("running_execution_id", successor.ID.String()).
				WithActions(output.NextAction{Label: "Wait for the running turn", Argv: []string{"agentctl", "await", successor.ID.String()}, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}})
		}
		if successor.State == model.StateCompleted {
			latest := successor
			for {
				next := continuationSuccessors(all, latest.ID)
				advanced := false
				for _, candidate := range next {
					if candidate.State == model.StateCompleted {
						latest, advanced = candidate, true
						break
					}
				}
				if !advanced {
					break
				}
			}
			return continueError(output.CodeConflict, "continue_not_latest", "this session already has a later completed turn; continue from that execution", source).WithDetail("latest_execution_id", latest.ID.String())
		}
	}
	plan := source.Delegation.NativePlan
	recipe := plan.Recipe()
	session := nativeSessionID(source)
	// Report a missing route before a missing session id: the route is the
	// reason an adapter such as Devin never records one.
	argv, err := adapter.ContinuationArgv(source.Adapter, recipe, plan.PromptDelivery, firstNonEmptyString(session, "unrecorded"))
	if err != nil {
		return mapAdapterError("this execution's native session cannot be continued", err).WithDetail("diagnostic_code", "continue_route_unavailable").WithDetail("execution_id", id.String()).WithDetail("adapter", source.Adapter)
	}
	if session == "" {
		return continueError(output.CodeCapabilityUnavailable, "continue_session_unrecorded", "this execution recorded no native session id, so its exact session cannot be continued", source).WithDetail("adapter", source.Adapter)
	}
	if source.CWD == nil || *source.CWD == "" {
		return continueError(output.CodeInvalidState, "continue_cwd_unrecorded", "the execution recorded no working directory", source)
	}
	if info, statErr := os.Stat(*source.CWD); statErr != nil || !info.IsDir() {
		return continueError(output.CodeInvalidState, "continue_cwd_missing", "the execution's working directory no longer exists; native sessions are resumed where they ran", source)
	}
	binding := &model.DelegationBinding{RequestSHA256: inputDigest, ConfigurationSHA256: source.Delegation.ConfigurationSHA256, Requested: append(json.RawMessage(nil), source.Delegation.Requested...),
		Resolved: source.Delegation.Resolved, Defaulted: append([]string(nil), source.Delegation.Defaulted...),
		NativePlan: &model.DelegationNativePlan{Argv: argv, PromptDelivery: plan.PromptDelivery, Permissions: plan.Permissions, RecipeArgv: append([]string(nil), recipe...)}}
	prompt.Delivery = plan.PromptDelivery
	labels := opts.labels
	if len(labels) == 0 {
		labels = append([]string(nil), source.Labels...)
	}
	admissionReused, admissionRecorded := false, false
	run := runOptions{adapter: source.Adapter, cwd: *source.CWD, argv: append([]string(nil), argv...), preparedPrompt: prompt, plan: opts.plan, timeout: opts.timeout, labels: labels,
		delegation: binding, supersedes: []ids.ExecutionID{id}, mutationOverride: &mutation, admissionReused: &admissionReused, admissionRecorded: &admissionRecorded}
	// Two invocations can both pass the checks above. Each journals its turn
	// first and then looks again, so exactly one of them launches: the turn
	// that was created first, with the execution ID as the tie-break.
	run.admit = func(created model.Execution) *output.Error {
		journal, problem := a.openRead(c)
		if problem != nil {
			return problem
		}
		all, err := journal.ListExecutions(context.Background(), false)
		journal.Close()
		if err != nil {
			return mapStoreError("recheck follow-up turns", err)
		}
		for _, other := range continuationSuccessors(all, id) {
			if other.ID == created.ID || (other.State.Terminal() && other.State != model.StateCompleted) {
				continue
			}
			if other.CreatedAt.Before(created.CreatedAt) || (other.CreatedAt.Equal(created.CreatedAt) && other.ID.String() < created.ID.String()) {
				return continueError(output.CodeConflict, "continue_in_progress", "another follow-up turn on this session was admitted first; this one was not launched", source).WithDetail("running_execution_id", other.ID.String())
			}
		}
		return nil
	}
	var captured bytes.Buffer
	problem = a.runNativeOptions(ctx, output.Renderer{Mode: output.JSON, Writer: &captured}, c, nil, run)
	if problem != nil {
		if execution, ok, _ := a.findDelegation(context.Background(), c, mutation); ok {
			problem.WithDetail("execution_id", execution.ID.String())
		}
		return problem.WithDetail("continues", id.String())
	}
	if opts.plan {
		var document struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(captured.Bytes(), &document); err != nil {
			return output.Wrap(output.CodeInternal, "decode native preflight", false, err)
		}
		return writeContinuePlan(renderer, id, *binding, map[string]any{"argv": redactSession(argv, session), "prompt_delivery": plan.PromptDelivery, "permissions": plan.Permissions, "preflight": document.Result}, "")
	}
	execution, found, problem := a.findDelegation(ctx, c, mutation)
	if problem != nil {
		return problem
	}
	if !found {
		return delegateError(output.CodeInternal, "continue_receipt_missing", "native launch returned without a continued execution receipt")
	}
	return a.collectContinuation(ctx, renderer, c, id, execution, opts, admissionReused)
}

// collectContinuation reports the new turn and, once it has completed, points
// the turn it continued at it.
func (a *app) collectContinuation(ctx context.Context, renderer output.Renderer, c common, source ids.ExecutionID, execution model.Execution, opts delegateOptions, reused bool) *output.Error {
	if execution.State == model.StateCompleted {
		a.markContinued(c, source, execution.ID)
	}
	return a.collectDelegation(ctx, renderer, c, execution, opts, reused)
}

// markContinued records the directed continuation on the superseded turn. The
// link on the new execution is authoritative; this back-pointer is a
// convenience for readers of the old record and is assigned at most once.
func (a *app) markContinued(c common, source, continuation ids.ExecutionID) {
	journal, problem := a.openWrite(c)
	if problem != nil {
		return
	}
	defer journal.Close()
	current, err := journal.GetExecution(context.Background(), source)
	if err != nil || current.SupersededBy != nil {
		return
	}
	now := a.now().UTC()
	current.SupersededBy = &continuation
	current.UpdatedAt = now
	updated, err := journal.UpdateExecution(context.Background(), current, current.Revision)
	if err != nil {
		return
	}
	event, canonical, err := syntheticEvent(updated, model.EventSuperseded, updated.State, map[string]any{"superseded_by": continuation.String(), "relation": "follow_up_turn"}, "continue:"+continuation.String(), now)
	if err != nil {
		return
	}
	_, _, _ = journal.AppendEvent(context.Background(), event, canonical)
}

// redactSession keeps the operator-private native session id out of plans.
func redactSession(argv []string, session string) []string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		if arg == session {
			arg = "<native-session>"
		}
		out[i] = arg
	}
	return out
}

func writeContinuePlan(renderer output.Renderer, source ids.ExecutionID, binding model.DelegationBinding, native any, reusedID string) *output.Error {
	result := map[string]any{"plan": true, "continues": source, "resolved": binding.Resolved,
		"lifecycle": map[string]any{"owner": "foreground_process", "restart_durable": false, "turn": "new execution that supersedes the continued one once it completes", "session": "same native session; the native CLI owns its history",
			"working_directory": "the continued execution's working directory", "collection": "--wait --content requires completed work and stored answer"},
		"request_sha256": binding.RequestSHA256, "side_effect_class": output.ReadOnly, "reused": reusedID != ""}
	if native != nil {
		result["native"] = native
	} else if binding.NativePlan != nil {
		result["native"] = map[string]any{"prompt_delivery": binding.NativePlan.PromptDelivery, "permissions": binding.NativePlan.Permissions, "source": "frozen_admission", "preflight_repeated": false}
	}
	if reusedID != "" {
		result["id"] = reusedID
	}
	if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "continue.plan", Fields: []output.Field{
		{Name: "continues", Value: source}, {Name: "harness", Value: binding.Resolved.Harness}, {Name: "model", Value: binding.Resolved.Model}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write continue plan", false, err)
	}
	return nil
}
