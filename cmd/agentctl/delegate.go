package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/delegation"
	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/route"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

type delegateOptions struct {
	requestFile, promptFile, cwd, authority string
	promptStdin, plan, wait, contentOnly    bool
	timeout                                 time.Duration
	labels                                  []string
	requireSource                           string
	minBytes                                int
}

func parseDelegate(args []string) (delegateOptions, *output.Error) {
	opts := delegateOptions{authority: "auto"}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if seen[flag] && flag != "--label" {
			return opts, delegateError(output.CodeUsage, "delegate_duplicate_flag", "delegate flag supplied more than once")
		}
		seen[flag] = true
		switch flag {
		case "--plan":
			opts.plan = true
		case "--wait":
			opts.wait = true
		case "--content":
			opts.contentOnly = true
		case "--prompt-stdin":
			opts.promptStdin = true
		case "--request-file", "--prompt-file", "--cwd", "--authority", "--timeout", "--label", "--require-result-source", "--min-result-bytes":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return opts, delegateError(output.CodeUsage, "delegate_missing_flag_value", "delegate flag requires a nonempty value").WithDetail("flag", flag)
			}
			i++
			value := args[i]
			switch flag {
			case "--request-file":
				opts.requestFile = value
			case "--prompt-file":
				opts.promptFile = value
			case "--cwd":
				opts.cwd = value
			case "--authority":
				opts.authority = value
			case "--timeout":
				v, err := time.ParseDuration(value)
				if err != nil || v <= 0 {
					return opts, delegateError(output.CodeUsage, "delegate_invalid_timeout", "timeout must be a positive Go duration")
				}
				opts.timeout = v
			case "--label":
				if !validRunLabel(value) || containsArg(opts.labels, value) || len(opts.labels) >= 16 {
					return opts, delegateError(output.CodeUsage, "delegate_invalid_label", "delegate requires at most 16 distinct exact labels")
				}
				opts.labels = append(opts.labels, value)
			case "--require-result-source":
				opts.requireSource = value
			case "--min-result-bytes":
				n, err := strconv.Atoi(value)
				if err != nil || n < 0 || n > 1<<20 {
					return opts, delegateError(output.CodeUsage, "delegate_invalid_result_bound", "minimum result bytes must be between 0 and 1048576")
				}
				opts.minBytes = n
			}
		default:
			return opts, delegateError(output.CodeUsage, "delegate_unknown_flag", "unknown delegate flag").WithDetail("flag", flag)
		}
	}
	if opts.requestFile == "" || boolCount(opts.promptFile != "", opts.promptStdin) != 1 {
		return opts, delegateError(output.CodeUsage, "delegate_input_required", "delegate requires --request-file and exactly one of --prompt-file or --prompt-stdin")
	}
	if opts.authority != "auto" && opts.authority != "native" && opts.authority != "multica" {
		return opts, delegateError(output.CodeUsage, "delegate_invalid_authority", "authority must be auto, native, or multica")
	}
	if (opts.contentOnly || opts.requireSource != "" || opts.minBytes > 0) && !opts.wait {
		return opts, delegateError(output.CodeUsage, "delegate_wait_required", "content and result assertions require --wait")
	}
	if opts.plan && (opts.contentOnly || opts.wait) {
		return opts, delegateError(output.CodeUsage, "delegate_plan_content_conflict", "--plan cannot be combined with --wait or result collection options")
	}
	sort.Strings(opts.labels)
	return opts, nil
}

func delegateError(code output.Code, diagnostic, message string) *output.Error {
	return output.NewError(code, message, false).WithDetail("diagnostic_code", diagnostic)
}

func readDelegateRequest(path string) (delegation.Request, *output.Error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return delegation.Request{}, delegateError(output.CodeUsage, "delegate_request_file", "request must be an existing regular non-symlink file of at most 64 KiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return delegation.Request{}, delegateError(output.CodeUsage, "delegate_request_file", "cannot read request file")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return delegation.Request{}, delegateError(output.CodeConflict, "delegate_request_changed", "request file changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return delegation.Request{}, delegateError(output.CodeUsage, "delegate_request_file", "cannot read request file")
	}
	request, err := delegation.DecodeRequest(body)
	if err != nil {
		return request, mapDelegationError(err)
	}
	return request, nil
}

func mapDelegationError(err error) *output.Error {
	var issue *delegation.Error
	if errors.As(err, &issue) {
		code := output.CodeUsage
		if issue.Kind == "ambiguous" {
			code = output.CodeAmbiguousReference
		}
		problem := delegateError(code, issue.Code, issue.Error())
		if len(issue.Candidates) != 0 {
			problem.WithDetail("candidates", issue.Candidates)
		}
		return problem
	}
	return delegateError(output.CodeUsage, "delegate_invalid_request", "invalid structured delegation request")
}

func delegateEntries(profile config.Profile) []delegation.Entry {
	var entries []delegation.Entry
	if profile.AgentPreferences == nil {
		return entries
	}
	for _, p := range profile.AgentPreferences.Preferred {
		harness := p.Agent
		if canonical, err := launchrecipe.CanonicalHarness(harness); err == nil {
			harness = canonical
		}
		entries = append(entries, delegation.Entry{Harness: harness, Family: p.Family, Version: p.Version, Model: p.Model,
			Speed: p.Speed, Effort: p.Effort, Aliases: route.ParseUseForAliases(p.UseFor), Default: p.Default})
	}
	return entries
}

func delegateExecutable(profile config.Profile, harness string) (string, *output.Error) {
	values := map[string]bool{}
	for name, item := range profile.Adapters {
		canonical, err := launchrecipe.CanonicalHarness(name)
		if err == nil && canonical == harness && item.Executable != "" {
			values[item.Executable] = true
		}
	}
	if len(values) > 1 {
		return "", delegateError(output.CodeAmbiguousReference, "delegate_ambiguous_executable", "harness aliases configure different executables")
	}
	for value := range values {
		return value, nil
	}
	return "", nil
}

func resolveDelegateHost(selector string, profile config.Profile) (string, bool, *output.Error) {
	canonical := func(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
	local := "local"
	if profile.Route != nil && strings.TrimSpace(profile.Route.ThisHost) != "" {
		local = profile.Route.ThisHost
	}
	if canonical(selector) == "local" {
		return local, true, nil
	}
	if selector == "" || canonical(selector) == canonical(local) {
		return local, true, nil
	}
	matched := map[string]bool{}
	hosts := map[string]string{}
	if profile.Route != nil {
		hosts = profile.Route.Hosts
	}
	for alias, host := range hosts {
		if host == "" {
			host = alias
		}
		if canonical(selector) == canonical(alias) || canonical(selector) == canonical(host) {
			matched[host] = true
		}
	}
	if len(matched) != 1 {
		code := output.CodeUsage
		if len(matched) > 1 {
			code = output.CodeAmbiguousReference
		}
		return "", false, delegateError(code, "delegate_host_unresolved", "host must resolve to exactly one configured host")
	}
	for host := range matched {
		return host, canonical(host) == canonical(local), nil
	}
	panic("unreachable")
}

func (a *app) delegateCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	opts, problem := parseDelegate(args)
	if problem != nil {
		return problem
	}
	if c.contextFile != "" {
		return delegateError(output.CodeCapabilityUnavailable, "delegate_context_unpinned", "structured delegation does not yet pin external context handles; supply bounded task context in the prompt")
	}
	request, problem := readDelegateRequest(opts.requestFile)
	if problem != nil {
		return problem
	}
	cwd, err := filepath.Abs(opts.cwd)
	if err != nil {
		return delegateError(output.CodeUsage, "delegate_invalid_cwd", "cannot resolve working directory")
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return delegateError(output.CodeUsage, "delegate_invalid_cwd", "working directory must exist")
	}
	opts.cwd = cwd
	prompt, problem := a.loadPrompt(runOptions{cwd: cwd, promptFile: opts.promptFile, promptStdin: opts.promptStdin, promptDelivery: "argv"})
	if problem != nil {
		return problem
	}
	if prompt == nil || len(bytes.TrimSpace(prompt.Bytes)) == 0 {
		return delegateError(output.CodeUsage, "delegate_empty_prompt", "delegate requires a nonempty task prompt")
	}
	profileName, profile, problem := a.resolveProfile(c)
	if problem != nil {
		return problem
	}
	inputDigest, err := digestJSON(map[string]any{"selector": canonicalDelegateSelector(request.Selector), "prompt_sha256": prompt.Digest,
		"cwd": cwd, "authority": opts.authority, "timeout": opts.timeout.String(), "labels": opts.labels})
	if err != nil {
		return output.Wrap(output.CodeInternal, "digest delegate inputs", false, err)
	}
	key, err := digestJSON(map[string]string{"profile": profileName, "key": request.RequestKey})
	if err != nil {
		return output.Wrap(output.CodeInternal, "digest delegate key", false, err)
	}
	mutation := contracts.MutationKey{Scope: "execution:delegate", Key: key, InputDigest: inputDigest}
	previous, found, problem := a.findDelegation(ctx, c, mutation)
	if problem != nil {
		return problem
	}
	if found {
		if previous.Delegation == nil {
			return delegateError(output.CodeInternal, "delegate_binding_missing", "delegated execution is missing its resolution binding").WithDetail("execution_id", previous.ID.String())
		}
		if opts.plan {
			return writeDelegatePlan(renderer, *previous.Delegation, nil, previous.ID.String())
		}
		return a.collectDelegation(ctx, renderer, c, previous, opts, true)
	}
	// Admission can race our first read. A local preflight failure must not
	// hide work another caller already reserved under this same semantic key.
	recoverAdmission := func(original *output.Error) *output.Error {
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		previous, found, lookupProblem := a.findDelegation(lookupCtx, c, mutation)
		if lookupProblem != nil {
			return lookupProblem
		}
		if !found {
			return original
		}
		if previous.Delegation == nil {
			return delegateExecutionError(output.CodeInternal, "delegated execution has no resolution binding", previous)
		}
		if opts.plan {
			return writeDelegatePlan(renderer, *previous.Delegation, nil, previous.ID.String())
		}
		return a.collectDelegation(ctx, renderer, c, previous, opts, true)
	}
	selector := request.Selector
	if canonical, err := launchrecipe.CanonicalHarness(selector.Harness); err == nil {
		selector.Harness = canonical
	}
	resolution, err := delegation.Resolve(selector, delegateEntries(profile))
	if err != nil {
		return recoverAdmission(mapDelegationError(err))
	}
	host, local, problem := resolveDelegateHost(request.Selector.Host, profile)
	if problem != nil {
		return recoverAdmission(problem)
	}
	if !local || opts.authority == "multica" {
		return recoverAdmission(delegateError(output.CodeCapabilityUnavailable, "delegate_multica_result_unavailable", "Multica does not yet expose the verified result and settings guarantees required by delegate; use explicit dispatch for lifecycle-only work").
			WithDetail("host", host).
			WithActions(output.NextAction{Label: "Inspect explicit Multica dispatch", Argv: []string{"agentctl", "help", "dispatch"}, SideEffectClass: output.ReadOnly}))
	}
	selected := resolution.Resolved
	permission := profile.Delegation != nil && profile.Delegation.CursorWorkspaceTrust
	executable, problem := delegateExecutable(profile, selected.Harness)
	if problem != nil {
		return recoverAdmission(problem)
	}
	recipe, err := launchrecipe.Build(launchrecipe.Input{Harness: selected.Harness, Model: selected.Model, Speed: selected.Speed, Effort: selected.Effort, Executable: executable, CursorWorkspaceTrust: permission})
	if err != nil {
		problem := delegateError(output.CodeCapabilityUnavailable, "delegate_recipe_unavailable", err.Error())
		var issue *launchrecipe.Error
		if errors.As(err, &issue) {
			problem.WithDetail("recipe_diagnostic", issue.Code)
		}
		return recoverAdmission(problem)
	}
	configDigest, err := digestJSON(profile)
	if err != nil {
		return output.Wrap(output.CodeInternal, "digest delegate configuration", false, err)
	}
	requested, _ := json.Marshal(request.Selector)
	defaulted := append([]string{}, resolution.Defaulted...)
	if request.Selector.Host == "" {
		defaulted = append(defaulted, "host")
	}
	binding := &model.DelegationBinding{RequestSHA256: inputDigest, ConfigurationSHA256: configDigest, Requested: requested, NativePlan: &model.DelegationNativePlan{Argv: append([]string(nil), recipe.Argv...), PromptDelivery: recipe.PromptDelivery},
		Resolved: model.DelegationTarget{Harness: selected.Harness, Family: selected.Family, Version: selected.Version, Model: selected.Model, Host: host, Authority: model.AuthorityNative,
			Settings: model.DelegationSettings{Speed: selected.Speed, Effort: selected.Effort}}, Defaulted: defaulted}
	prompt.Delivery = recipe.PromptDelivery
	admissionReused := false
	admissionRecorded := false
	run := runOptions{adapter: selected.Harness, cwd: cwd, argv: append([]string(nil), recipe.Argv...), preparedPrompt: prompt,
		plan: opts.plan, timeout: opts.timeout, labels: opts.labels, delegation: binding, mutationOverride: &mutation, admissionReused: &admissionReused, admissionRecorded: &admissionRecorded}
	var captured bytes.Buffer
	inner := output.Renderer{Mode: output.JSON, Writer: &captured}
	problem = a.runNativeOptions(ctx, inner, c, nil, run)
	if problem != nil {
		if !admissionRecorded {
			return recoverAdmission(problem)
		}
		if execution, ok, _ := a.findDelegation(context.Background(), c, mutation); ok {
			problem.WithDetail("execution_id", execution.ID.String())
		}
		return problem
	}
	if opts.plan {
		var document struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(captured.Bytes(), &document); err != nil {
			return output.Wrap(output.CodeInternal, "decode native preflight", false, err)
		}
		return writeDelegatePlan(renderer, *binding, map[string]any{"argv": recipe.Argv, "prompt_delivery": recipe.PromptDelivery, "preflight": document.Result}, "")
	}
	execution, found, problem := a.findDelegation(ctx, c, mutation)
	if problem != nil {
		return problem
	}
	if !found {
		return delegateError(output.CodeInternal, "delegate_receipt_missing", "native launch returned without a delegated execution receipt")
	}
	return a.collectDelegation(ctx, renderer, c, execution, opts, admissionReused)
}

// Canonicalize identifier spelling for replay while preserving the original
// user constraints in the immutable binding. Native model IDs remain case-sensitive.
func canonicalDelegateSelector(in delegation.Selector) delegation.Selector {
	in.Harness = strings.ToLower(strings.TrimSpace(in.Harness))
	if canonical, err := launchrecipe.CanonicalHarness(in.Harness); err == nil {
		in.Harness = canonical
	}
	in.Family = strings.ToLower(strings.TrimSpace(in.Family))
	in.Version = strings.ToLower(strings.TrimSpace(in.Version))
	in.Host = strings.ToLower(strings.TrimSpace(in.Host))
	in.Model = strings.TrimSpace(in.Model)
	if in.Settings != nil {
		settings := *in.Settings
		settings.Speed = strings.ToLower(strings.TrimSpace(settings.Speed))
		settings.Effort = strings.ToLower(strings.TrimSpace(settings.Effort))
		if settings.Speed == "" && settings.Effort == "" {
			in.Settings = nil
		} else {
			in.Settings = &settings
		}
	}
	return in
}

func digestJSON(value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return sha256Digest(body), nil
}

func (a *app) findDelegation(ctx context.Context, c common, mutation contracts.MutationKey) (model.Execution, bool, *output.Error) {
	path, err := a.journalPath(c)
	if err != nil {
		return model.Execution{}, false, output.Wrap(output.CodeInternal, "resolve journal", false, err)
	}
	journal, err := openJournalWithRetryContext(ctx, path, store.Options{ReadOnly: true})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return model.Execution{}, false, nil
	}
	if err != nil {
		return model.Execution{}, false, mapStoreError("read delegated request", err)
	}
	defer journal.Close()
	execution, found, err := journal.GetExecutionByMutation(ctx, mutation)
	if err != nil {
		return execution, false, mapStoreError("recover delegated request", err)
	}
	return execution, found, nil
}

func writeDelegatePlan(renderer output.Renderer, binding model.DelegationBinding, native any, reusedID string) *output.Error {
	result := map[string]any{"plan": true, "requested": binding.Requested, "resolved": binding.Resolved,
		"provenance":     map[string]any{"configuration_sha256": binding.ConfigurationSHA256, "defaulted": binding.Defaulted},
		"request_sha256": binding.RequestSHA256, "side_effect_class": output.ReadOnly, "reused": reusedID != ""}
	if native != nil {
		result["native"] = native
	} else if binding.NativePlan != nil {
		result["native"] = map[string]any{"argv": binding.NativePlan.Argv, "prompt_delivery": binding.NativePlan.PromptDelivery, "source": "frozen_admission", "preflight_repeated": false}
	}
	if reusedID != "" {
		result["id"] = reusedID
	}
	err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "delegate.plan", Fields: []output.Field{
		{Name: "harness", Value: binding.Resolved.Harness}, {Name: "model", Value: binding.Resolved.Model}, {Name: "host", Value: binding.Resolved.Host}, {Name: "authority", Value: binding.Resolved.Authority}}}}})
	if err != nil {
		return output.Wrap(output.CodeInternal, "write delegation plan", false, err)
	}
	return nil
}

func (a *app) collectDelegation(ctx context.Context, renderer output.Renderer, c common, execution model.Execution, opts delegateOptions, reused bool) *output.Error {
	for opts.wait && !execution.State.Terminal() {
		if execution.State == model.StateAttention || execution.Liveness == model.LivenessBlocked {
			return delegateExecutionError(output.CodeAttentionRequired, "delegated execution requires attention", execution)
		}
		if execution.Liveness == model.LivenessUnreachable || (len(execution.SourceBindings) == 0 && a.now().Sub(execution.CreatedAt) > time.Minute) {
			return delegateExecutionError(output.CodeExecutionUnknown, "delegated launch or execution cannot be proven; it has not been relaunched", execution)
		}
		if fresh := execution.Observation.FreshForSeconds; fresh != nil && a.now().After(execution.Observation.ObservedAt.Add(time.Duration(*fresh)*time.Second+10*time.Second)) {
			return delegateExecutionError(output.CodeExecutionUnknown, "delegated owner lease expired; execution has not been relaunched", execution)
		}
		if execution.DeadlineAt != nil && a.now().After(execution.DeadlineAt.Add(10*time.Second)) {
			return delegateExecutionError(output.CodeTimeout, "delegated execution deadline elapsed without a terminal outcome", execution)
		}
		select {
		case <-ctx.Done():
			return delegateExecutionError(output.CodeTimeout, "stopped waiting for delegated execution", execution)
		case <-time.After(250 * time.Millisecond):
		}
		journal, problem := a.openRead(c)
		if problem != nil {
			return problem.WithDetail("execution_id", execution.ID.String())
		}
		latest, err := journal.GetExecution(ctx, execution.ID)
		journal.Close()
		if err != nil {
			return mapStoreError("observe delegated execution", err).WithDetail("execution_id", execution.ID.String())
		}
		execution = latest
	}
	var outcome *model.Outcome
	if opts.wait {
		if execution.Observation.Integrity == model.IntegrityConflicted {
			return delegateExecutionError(output.CodeExecutionUnknown, "delegated outcome has conflicting evidence", execution)
		}
		switch execution.State {
		case model.StateFailed:
			return delegateExecutionError(output.CodeExecutionFailed, "delegated execution failed", execution)
		case model.StateCancelled:
			return delegateExecutionError(output.CodeExecutionCancelled, "delegated execution was cancelled", execution)
		case model.StateOrphaned:
			return delegateExecutionError(output.CodeExecutionUnknown, "delegated execution is orphaned", execution)
		}
		journal, problem := a.openRead(c)
		if problem != nil {
			return problem.WithDetail("execution_id", execution.ID.String())
		}
		_, value, problem := dereferenceResult(ctx, journal, execution.ID, resultOptions{requireContent: true, requireSource: opts.requireSource, minResultBytes: opts.minBytes})
		journal.Close()
		if problem != nil {
			return problem.WithDetail("execution_id", execution.ID.String())
		}
		if value.Content == nil || strings.TrimSpace(value.Content.Text) == "" {
			return delegateExecutionError(output.CodeInvalidState, "delegated execution has no nonempty final answer", execution)
		}
		outcome = &value
	}
	binding := execution.Delegation
	if binding == nil {
		return delegateExecutionError(output.CodeInternal, "delegated execution has no resolution binding", execution)
	}
	if opts.contentOnly {
		if _, err := io.WriteString(renderer.Writer, outcome.Content.Text); err != nil {
			return output.Wrap(output.CodeInternal, "write delegated answer", false, err)
		}
	} else {
		result := map[string]any{"id": execution.ID, "origin_host_id": execution.OriginHostID, "state": execution.State, "liveness": execution.Liveness,
			"authority": execution.Authority, "reused": reused, "requested": binding.Requested, "resolved": binding.Resolved,
			"provenance": map[string]any{"configuration_sha256": binding.ConfigurationSHA256, "defaulted": binding.Defaulted}, "request_sha256": binding.RequestSHA256}
		if outcome != nil {
			result["outcome"] = outcome
		}
		actions := []output.NextAction{{Label: "Retrieve final answer", Argv: []string{"agentctl", "result", execution.ID.String()}, Mutates: true, SideEffectClass: output.LocalOperationalWrite}}
		if !execution.State.Terminal() {
			actions = append([]output.NextAction{{Label: "Wait for execution", Argv: []string{"agentctl", "await", execution.ID.String()}, Mutates: true, SideEffectClass: output.LocalOperationalWrite}}, actions...)
		}
		if err := renderer.Success(output.Success{Result: result, NextActions: actions, Lines: []output.Line{{Lead: execution.ID.String(), Fields: []output.Field{{Name: "state", Value: execution.State}, {Name: "harness", Value: binding.Resolved.Harness}, {Name: "model", Value: binding.Resolved.Model}, {Name: "host", Value: binding.Resolved.Host}, {Name: "reused", Value: reused}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write delegated result", false, err)
		}
	}
	if opts.wait {
		if problem := a.acknowledgeExecution(ctx, c, execution.ID, store.AcknowledgementResult); problem != nil {
			_, _ = io.WriteString(a.stderr, "agentctl: answer delivered but collection acknowledgement failed\n")
		}
	}
	return nil
}

func delegateExecutionError(code output.Code, message string, execution model.Execution) *output.Error {
	return delegateError(code, "delegate_"+string(code), message).WithDetail("execution_id", execution.ID.String()).WithDetail("state", execution.State).
		WithActions(output.NextAction{Label: "Inspect execution", Argv: []string{"agentctl", "status", execution.ID.String()}, SideEffectClass: output.ReadOnly})
}
