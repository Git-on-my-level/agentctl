package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	"github.com/Git-on-my-level/agentctl/internal/route"
)

type dispatchOptions struct {
	routeSelector  string
	title          string
	promptFile     string
	promptStdin    bool
	project        string
	status         string
	idempotencyKey string
	labels         []string
	plan           bool
}

type multicaAgentRecord struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Model      string  `json:"model"`
	RuntimeID  string  `json:"runtime_id"`
	Status     string  `json:"status"`
	ArchivedAt *string `json:"archived_at"`
}

type multicaRuntimeRecord struct {
	ID         string `json:"id"`
	CustomName string `json:"custom_name"`
	Provider   string `json:"provider"`
	Status     string `json:"status"`
}

type dispatchRoute struct {
	Host    string `json:"host"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
}

type dispatchTarget struct {
	AgentID     string        `json:"-"`
	AgentName   string        `json:"agent_name"`
	RuntimeID   string        `json:"-"`
	RuntimeName string        `json:"runtime_name"`
	Route       dispatchRoute `json:"route"`
}

func (a *app) dispatchCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	opts, problem := parseDispatch(args)
	if problem != nil {
		return problem
	}
	prompt, problem := a.loadPrompt(runOptions{promptFile: opts.promptFile, promptStdin: opts.promptStdin, promptDelivery: "stdin"})
	if problem != nil {
		return problem
	}
	if prompt == nil {
		return output.NewError(output.CodeUsage, "dispatch requires --prompt-file or --prompt-stdin", false)
	}
	if len(prompt.Bytes) > maxPromotionInputBytes {
		return output.NewError(output.CodeUsage, "dispatch prompt exceeds 1 MiB limit", false).WithDetail("max_bytes", maxPromotionInputBytes)
	}
	if !utf8.Valid(prompt.Bytes) || bytes.IndexByte(prompt.Bytes, 0) >= 0 {
		return output.NewError(output.CodeUsage, "dispatch prompt must be valid UTF-8 without NUL bytes", false)
	}
	profileName, profile, problem := a.resolveProfile(c)
	if problem != nil {
		return problem
	}
	if profile.Multica == nil {
		return output.NewError(output.CodeCapabilityUnavailable, "selected profile has no Multica authority", false).WithDetail("profile", profileName)
	}
	if profile.Route == nil || profile.Route.Placement == nil || strings.TrimSpace(profile.Route.Placement.Kind) != "multica" {
		return output.NewError(output.CodeCapabilityUnavailable, "selected profile does not route remote work through Multica", false).WithDetail("profile", profileName)
	}
	catalog := catalogFromProfile(profile)
	selected, problem := resolveDispatchRoute(opts.routeSelector, catalog)
	if problem != nil {
		return problem
	}
	provenance, warnings, problem := a.promotionProvenance(c.contextFile, prompt.Bytes)
	if problem != nil {
		return problem
	}
	dispatchStatus := strings.TrimSpace(opts.status)
	if dispatchStatus == "" {
		dispatchStatus = "todo"
	}
	description := buildDispatchDescription(prompt.Bytes, provenance, dispatchStatus)
	if len(description) > maxPromotionInputBytes {
		return output.NewError(output.CodeUsage, "dispatch description exceeds 1 MiB limit after provenance", false).WithDetail("max_bytes", maxPromotionInputBytes)
	}
	m := profile.Multica
	keyProjection := map[string]any{
		"destination": map[string]any{"server_url": m.ServerURL, "workspace_id": m.WorkspaceID},
		"caller_key":  opts.idempotencyKey,
	}
	clientKey, _, err := callback.SemanticDedupeKey("multica-dispatch", 1, keyProjection)
	if err != nil {
		return output.Wrap(output.CodeInternal, "derive dispatch key", false, err)
	}
	semanticProjection := map[string]any{
		"route": selected, "title": opts.title,
		"prompt_digest": prompt.Digest, "project": opts.project, "status": dispatchStatus,
		"labels": opts.labels,
	}
	semanticBytes, err := callback.CanonicalJSON(semanticProjection)
	if err != nil {
		return output.Wrap(output.CodeInternal, "derive dispatch input digest", false, err)
	}
	inputDigest := sha256Digest(semanticBytes)
	mutation := contracts.MutationKey{Scope: "dispatch:create", Key: clientKey, InputDigest: inputDigest}
	if opts.plan {
		target, problem := resolveMulticaDispatchTarget(ctx, m, catalog, selected)
		if problem != nil {
			return problem.WithDetail("profile", profileName)
		}
		publicTarget := publicDispatchTarget(target)
		result := map[string]any{
			"authority": "multica", "profile": profileName, "target": publicTarget,
			"client_key": clientKey, "prompt_digest": prompt.Digest, "runtime_verified": true,
			"dispatch_status": dispatchStatus, "tracked_execution": false, "side_effect_class": output.ReadOnly,
		}
		lines := []output.Line{{Lead: "dispatch.plan", Fields: []output.Field{{Name: "host", Value: selected.Host}, {Name: "adapter", Value: selected.Adapter}, {Name: "model", Value: selected.Model}, {Name: "agent", Value: target.AgentName}, {Name: "runtime_verified", Value: true}}}}
		if err := renderer.Success(output.Success{Result: result, Warnings: warnings, Lines: lines}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	}
	journal, problem := a.openWrite(c)
	if problem != nil {
		return problem.WithDetail("client_key", clientKey)
	}
	defer journal.Close()
	prepared, found, err := journal.GetExecutionByMutation(ctx, mutation)
	if err != nil {
		return mapStoreError("recover prepared dispatch", err).WithDetail("client_key", clientKey)
	}
	reused := found
	runtimeVerifiedNow := false
	target := dispatchTarget{Route: selected}
	if found {
		target, err = dispatchTargetFromBindings(prepared, selected)
		if err != nil {
			return output.Wrap(output.CodeInternal, "recover prepared dispatch target", false, err).WithDetail("execution_id", prepared.ID.String())
		}
	} else {
		target, problem = resolveMulticaDispatchTarget(ctx, m, catalog, selected)
		if problem != nil {
			return problem.WithDetail("profile", profileName)
		}
		runtimeVerifiedNow = true
		now := a.now().UTC()
		bindings, bindingErr := dispatchPreparedBindings(m, target)
		if bindingErr != nil {
			return output.Wrap(output.CodeInternal, "create prepared dispatch aliases", false, bindingErr)
		}
		candidate := model.Execution{
			Authority: model.AuthorityMultica, Adapter: "multica", Mode: model.ModeMultica,
			Acquisition: model.AcquisitionLaunched, State: model.StateStarting, Liveness: model.LivenessUnknown,
			SourceState: dispatchStringPointer("dispatch_prepared"), SourceBindings: bindings,
			Capabilities: dispatchPreparedCapabilities(now), Labels: append([]string(nil), opts.labels...), Supersedes: []ids.ExecutionID{},
			TaskContract: &model.TaskContract{ObjectiveSummary: opts.title, SideEffectBoundary: "multica_issue", Provenance: &provenance},
			Observation:  model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityVerified, ObservedAt: now},
		}
		prepared, reused, err = journal.CreateExecution(ctx, candidate, mutation)
		if err != nil {
			return mapStoreError("prepare dispatched execution", err).WithDetail("client_key", clientKey)
		}
		if reused {
			target, err = dispatchTargetFromBindings(prepared, selected)
			if err != nil {
				return output.Wrap(output.CodeInternal, "recover raced dispatch target", false, err).WithDetail("execution_id", prepared.ID.String())
			}
		}
	}
	issueID, issueBound := dispatchBindingOpaque(prepared.SourceBindings, "multica_issue")
	identifier := ""
	if !issueBound {
		argv := multicaBaseArgv(m, "issue", "create")
		argv = append(argv, "--title", opts.title, "--client-key", clientKey, "--description-stdin", "--output", "json", "--assignee-id", target.AgentID, "--status", "backlog")
		if opts.project != "" {
			argv = append(argv, "--project", opts.project)
		}
		issue, createErr := runMulticaIssueCreate(ctx, argv, description)
		if createErr != nil {
			if errors.Is(createErr, errMulticaIssueConflict) {
				return output.Wrap(output.CodeConflict, "Multica issue client key conflicts with changed dispatch semantics", false, createErr).WithDetail("client_key", clientKey).WithDetail("execution_id", prepared.ID.String()).WithDetail("profile", profileName)
			}
			return output.Wrap(output.CodeRemoteFailure, "create or recover Multica dispatch issue", true, createErr).WithDetail("client_key", clientKey).WithDetail("execution_id", prepared.ID.String()).WithDetail("profile", profileName)
		}
		issueID, _ = issue["id"].(string)
		identifier, _ = issue["identifier"].(string)
		if strings.TrimSpace(issueID) == "" {
			return output.NewError(output.CodeRemoteFailure, "Multica response omitted issue ID", true).WithDetail("client_key", clientKey).WithDetail("execution_id", prepared.ID.String())
		}
		if prepared.State != model.StateStarting {
			return output.NewError(output.CodeInvalidState, "prepared dispatch changed before authority binding", false).WithDetail("execution_id", prepared.ID.String()).WithDetail("state", prepared.State)
		}
		issueBinding, bindingErr := newDispatchBinding("multica_issue", issueID, ids.TypeIssue)
		if bindingErr != nil {
			return output.Wrap(output.CodeInternal, "create dispatch issue alias", false, bindingErr)
		}
		now := a.now().UTC()
		prepared.SourceBindings = append(prepared.SourceBindings, issueBinding)
		prepared.SourceState = dispatchStringPointer("issue_bound")
		prepared.Capabilities = promotedCapabilities(now)
		prepared.UpdatedAt = now
		prepared.Observation = model.Observation{Source: model.ObservationDurableOutbox, Integrity: model.IntegrityVerified, ObservedAt: now}
		prepared, err = journal.UpdateExecution(ctx, prepared, prepared.Revision)
		if err != nil {
			return mapStoreError("bind dispatched execution", err).WithDetail("client_key", clientKey).WithDetail("execution_id", prepared.ID.String()).WithDetail("multica_identifier", identifier)
		}
	}
	var currentIssue map[string]any
	if err := runMulticaJSON(ctx, multicaBaseArgv(m, "issue", "get", issueID, "--output", "json"), &currentIssue); err != nil {
		return output.Wrap(output.CodeRemoteFailure, "read tracked Multica dispatch issue", true, err).WithDetail("client_key", clientKey).WithDetail("execution_id", prepared.ID.String()).WithDetail("multica_identifier", identifier)
	}
	if currentIdentifier, _ := currentIssue["identifier"].(string); currentIdentifier != "" {
		identifier = currentIdentifier
	}
	currentStatus, _ := currentIssue["status"].(string)
	currentStatus = strings.ToLower(strings.TrimSpace(currentStatus))
	if currentStatus == "" {
		return output.NewError(output.CodeRemoteFailure, "Multica response omitted issue status", true).WithDetail("client_key", clientKey).WithDetail("execution_id", prepared.ID.String()).WithDetail("multica_identifier", identifier)
	}
	if currentStatus == "backlog" && dispatchStatus != "backlog" {
		updateArgv := multicaBaseArgv(m, "issue", "update", issueID, "--status", dispatchStatus, "--output", "json")
		if _, err := runMulticaIssueUpdate(ctx, updateArgv); err != nil {
			return output.Wrap(output.CodeRemoteFailure, "activate recovered Multica dispatch issue", true, err).WithDetail("client_key", clientKey).WithDetail("execution_id", prepared.ID.String()).WithDetail("multica_identifier", identifier)
		}
	}
	if prepared.State == model.StateStarting {
		now := a.now().UTC()
		prepared.State = model.StateWaiting
		prepared.Liveness = model.LivenessUnknown
		prepared.SourceState = dispatchStringPointer("issue_created")
		prepared.UpdatedAt = now
		prepared.Observation = model.Observation{Source: model.ObservationDurableOutbox, Integrity: model.IntegrityVerified, ObservedAt: now}
		prepared, err = journal.UpdateExecution(ctx, prepared, prepared.Revision)
		if err != nil {
			return mapStoreError("finalize dispatched execution", err).WithDetail("client_key", clientKey).WithDetail("execution_id", prepared.ID.String()).WithDetail("multica_identifier", identifier)
		}
	}
	created := prepared
	publicTarget := publicDispatchTarget(target)
	issueAlias, ok := bindingAlias(created.SourceBindings, "multica_issue")
	if !ok {
		return output.NewError(output.CodeInternal, "dispatched execution omitted its issue alias", false).WithDetail("execution_id", created.ID.String())
	}
	agentAlias, ok := bindingAlias(created.SourceBindings, "multica_assignee")
	if !ok {
		return output.NewError(output.CodeInternal, "dispatched execution omitted its assignee alias", false).WithDetail("execution_id", created.ID.String())
	}
	result := map[string]any{
		"execution": redactedExecution(created), "issue_alias": issueAlias, "assignee_alias": agentAlias,
		"identifier": identifier, "app_url": m.AppURL, "client_key": clientKey,
		"prompt_digest": prompt.Digest, "target": publicTarget, "runtime_verified_now": runtimeVerifiedNow, "runtime_verified_at_prepare": true,
		"dispatch_status": dispatchStatus, "tracked_execution": true, "reused": reused,
	}
	lines := []output.Line{{Lead: created.ID.String(), Fields: []output.Field{{Name: "authority", Value: "multica"}, {Name: "state", Value: created.State}, {Name: "host", Value: selected.Host}, {Name: "agent", Value: target.AgentName}, {Name: "issue", Value: issueAlias}, {Name: "identifier", Value: identifier}, {Name: "reused", Value: reused}}}}
	actions := []output.NextAction{
		{Label: "Wait for Multica progress", Argv: []string{"agentctl", "await", created.ID.String(), "--output", string(renderer.Mode)}, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}},
		{Label: "Inspect cached status", Argv: []string{"agentctl", "status", created.ID.String(), "--output", string(renderer.Mode)}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}},
		{Label: "Discover durable callback setup", Argv: []string{"agentctl", "help", "subscribe"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"choose an explicit callback destination and target"}},
	}
	if err := renderer.Success(output.Success{Result: result, Warnings: warnings, Lines: lines, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func parseDispatch(args []string) (dispatchOptions, *output.Error) {
	opts := dispatchOptions{}
	for i := 0; i < len(args); i++ {
		take := func() (string, *output.Error) {
			if i+1 >= len(args) {
				return "", output.NewError(output.CodeUsage, args[i]+" requires a value", false)
			}
			i++
			return strings.TrimSpace(args[i]), nil
		}
		switch args[i] {
		case "--route":
			value, problem := take()
			if problem != nil {
				return opts, problem
			}
			opts.routeSelector = value
		case "--title":
			value, problem := take()
			if problem != nil {
				return opts, problem
			}
			opts.title = value
		case "--prompt-file":
			value, problem := take()
			if problem != nil {
				return opts, problem
			}
			opts.promptFile = value
		case "--prompt-stdin":
			opts.promptStdin = true
		case "--project":
			value, problem := take()
			if problem != nil {
				return opts, problem
			}
			opts.project = value
		case "--status":
			value, problem := take()
			if problem != nil {
				return opts, problem
			}
			opts.status = strings.ToLower(value)
			if !validDispatchStatus(opts.status) {
				return opts, output.NewError(output.CodeUsage, "--status must be backlog, todo, in_progress, in_review, done, blocked, or cancelled", false).WithDetail("status", value)
			}
		case "--idempotency-key":
			value, problem := take()
			if problem != nil {
				return opts, problem
			}
			opts.idempotencyKey = value
		case "--label":
			value, problem := take()
			if problem != nil {
				return opts, problem
			}
			if !validRunLabel(value) {
				return opts, output.NewError(output.CodeUsage, "--label must match ^[a-z][a-z0-9_.-]{0,63}$", false).WithDetail("label", value)
			}
			if containsArg(opts.labels, value) {
				return opts, output.NewError(output.CodeUsage, "--label values must be unique", false).WithDetail("label", value)
			}
			opts.labels = append(opts.labels, value)
			if len(opts.labels) > 16 {
				return opts, output.NewError(output.CodeUsage, "dispatch accepts at most 16 labels", false)
			}
		case "--plan":
			opts.plan = true
		default:
			return opts, output.NewError(output.CodeUsage, "unknown dispatch flag", false).WithDetail("flag", args[i])
		}
	}
	if opts.routeSelector == "" {
		return opts, output.NewError(output.CodeUsage, "--route is required", false)
	}
	if opts.title == "" {
		return opts, output.NewError(output.CodeUsage, "--title is required", false)
	}
	if len(opts.title) > 2048 {
		return opts, output.NewError(output.CodeUsage, "--title exceeds 2048 bytes", false)
	}
	if opts.idempotencyKey == "" {
		return opts, output.NewError(output.CodeUsage, "--idempotency-key is required for replay-safe remote dispatch", false)
	}
	if len(opts.idempotencyKey) > 256 {
		return opts, output.NewError(output.CodeUsage, "--idempotency-key exceeds 256 bytes", false)
	}
	if opts.promptFile != "" && opts.promptStdin {
		return opts, output.NewError(output.CodeUsage, "--prompt-file and --prompt-stdin are mutually exclusive", false)
	}
	if opts.promptFile == "" && !opts.promptStdin {
		return opts, output.NewError(output.CodeUsage, "dispatch requires --prompt-file or --prompt-stdin", false)
	}
	return opts, nil
}

func validDispatchStatus(value string) bool {
	switch value {
	case "backlog", "todo", "in_progress", "in_review", "done", "blocked", "cancelled":
		return true
	default:
		return false
	}
}

func resolveDispatchRoute(selector string, catalog route.Catalog) (dispatchRoute, *output.Error) {
	matched := route.Match(selector, catalog)
	policyLocked := route.HasConcretePreferences(catalog)
	if len(matched.Unmatched) != 0 {
		if policyLocked {
			return dispatchRoute{}, output.NewError(output.CodeUsage, "dispatch route is not in the active profile's agent_preferences table", false).WithDetail("tokens", matched.Unmatched)
		}
		return dispatchRoute{}, output.NewError(output.CodeUsage, "dispatch route contains unmatched tokens", false).WithDetail("tokens", matched.Unmatched)
	}
	host, ok := uniqueTopDispatchHost(matched.Hosts)
	if !ok {
		return dispatchRoute{}, output.NewError(output.CodeUsage, "dispatch route must identify exactly one configured host", false)
	}
	modelHit, ok := uniqueTopDispatchModel(matched.Models)
	if !ok || strings.TrimSpace(modelHit.Model) == "" {
		message := "dispatch route must identify exactly one configured concrete model"
		if policyLocked {
			message = "dispatch route must identify exactly one model from the active profile's agent_preferences table"
		}
		return dispatchRoute{}, output.NewError(output.CodeUsage, message, false)
	}
	if policyLocked && !route.InPreferredTable(catalog.Models, modelHit.Adapter, modelHit.Model) {
		return dispatchRoute{}, output.NewError(output.CodeUsage, "dispatch route is not in the active profile's agent_preferences table", false).WithDetail("adapter", modelHit.Adapter).WithDetail("model", modelHit.Model)
	}
	return dispatchRoute{Host: host, Adapter: modelHit.Adapter, Model: modelHit.Model}, nil
}

func uniqueTopDispatchHost(hits []route.HostHit) (string, bool) {
	if len(hits) == 0 {
		return "", false
	}
	top := hits[0].Score
	seen := map[string]bool{}
	for _, hit := range hits {
		if hit.Score != top {
			break
		}
		seen[hit.ID] = true
	}
	if len(seen) != 1 {
		return "", false
	}
	for value := range seen {
		return value, true
	}
	return "", false
}

func uniqueTopDispatchModel(hits []route.ModelHit) (route.ModelHit, bool) {
	if len(hits) == 0 {
		return route.ModelHit{}, false
	}
	top := hits[0].Score
	var selected route.ModelHit
	count := 0
	for _, hit := range hits {
		if hit.Score != top {
			break
		}
		selected, count = hit, count+1
	}
	return selected, count == 1
}

func resolveMulticaDispatchTarget(ctx context.Context, m *config.Multica, catalog route.Catalog, selected dispatchRoute) (dispatchTarget, *output.Error) {
	var agents []multicaAgentRecord
	if err := runMulticaJSON(ctx, multicaBaseArgv(m, "agent", "list", "--output", "json"), &agents); err != nil {
		return dispatchTarget{}, output.Wrap(output.CodeRemoteFailure, "list Multica agents for dispatch", true, err)
	}
	var runtimes []multicaRuntimeRecord
	if err := runMulticaJSON(ctx, multicaBaseArgv(m, "runtime", "list", "--output", "json"), &runtimes); err != nil {
		return dispatchTarget{}, output.Wrap(output.CodeRemoteFailure, "list Multica runtimes for dispatch", true, err)
	}
	byID := make(map[string]multicaRuntimeRecord, len(runtimes))
	for _, runtime := range runtimes {
		byID[runtime.ID] = runtime
	}
	var matches []dispatchTarget
	for _, agent := range agents {
		if agent.ArchivedAt != nil || strings.TrimSpace(agent.ID) == "" || strings.TrimSpace(agent.RuntimeID) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(agent.Status)) {
		case "idle", "working":
		default:
			continue
		}
		runtime, ok := byID[agent.RuntimeID]
		if !ok || !strings.EqualFold(strings.TrimSpace(runtime.Status), "online") {
			continue
		}
		if canonicalProvider(runtime.Provider) != canonicalProvider(selected.Adapter) {
			continue
		}
		hostMatch := route.Match(runtime.CustomName, catalog)
		host, ok := uniqueTopDispatchHost(hostMatch.Hosts)
		if !ok || host != selected.Host {
			continue
		}
		modelMatch := route.Match(agent.Model, catalog)
		modelHit, ok := uniqueTopDispatchModel(modelMatch.Models)
		if !ok || canonicalProvider(modelHit.Adapter) != canonicalProvider(selected.Adapter) || modelHit.Model != selected.Model {
			continue
		}
		matches = append(matches, dispatchTarget{AgentID: agent.ID, AgentName: agent.Name, RuntimeID: runtime.ID, RuntimeName: runtime.CustomName, Route: selected})
	}
	if len(matches) == 0 {
		return dispatchTarget{}, output.NewError(output.CodeCapabilityUnavailable, "no online Multica agent matches the routed host, adapter, and model", false).WithDetail("host", selected.Host).WithDetail("adapter", selected.Adapter).WithDetail("model", selected.Model)
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.AgentName)
		}
		return dispatchTarget{}, output.NewError(output.CodeConflict, "multiple online Multica agents match the routed host, adapter, and model", false).WithDetail("host", selected.Host).WithDetail("adapter", selected.Adapter).WithDetail("model", selected.Model).WithDetail("agents", names)
	}
	return matches[0], nil
}

func canonicalProvider(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude-code":
		return "claude"
	case "generic-process":
		return "generic"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func multicaBaseArgv(m *config.Multica, command ...string) []string {
	argv := []string{m.Executable}
	if m.Profile != adapter.MulticaDefaultProfile {
		argv = append(argv, "--profile", m.Profile)
	}
	argv = append(argv, "--workspace-id", m.WorkspaceID)
	if m.ServerURL != "" {
		argv = append(argv, "--server-url", m.ServerURL)
	}
	return append(argv, command...)
}

func runMulticaJSON(ctx context.Context, argv []string, destination any) error {
	if len(argv) == 0 {
		return errors.New("Multica argv is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	var stdout, stderr boundedBuffer
	stdout.limit = 4 << 20
	stderr.limit = 64 << 10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Multica read failed (%T)", err)
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(destination); err != nil {
		return errors.New("Multica returned invalid JSON")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("Multica returned trailing output")
	}
	return nil
}

func buildDispatchDescription(prompt []byte, provenance model.ExecutionProvenance, dispatchStatus string) []byte {
	var value bytes.Buffer
	value.Write(bytes.TrimSpace(prompt))
	value.WriteString("\n\nAgentctl provenance:\n")
	for _, item := range []struct{ name, digest string }{
		{"portable_skill_digest", provenance.PortableSkillDigest}, {"context_digest", provenance.ContextDigest}, {"prompt_digest", provenance.HandoffDigest},
	} {
		if item.digest == "" {
			continue
		}
		value.WriteString(item.name)
		value.WriteString(": ")
		value.WriteString(item.digest)
		value.WriteByte('\n')
	}
	value.WriteString("dispatch_status: ")
	value.WriteString(dispatchStatus)
	value.WriteByte('\n')
	return value.Bytes()
}

func dispatchPreparedBindings(m *config.Multica, target dispatchTarget) ([]model.SourceBinding, error) {
	items := []struct {
		kind, value string
		typ         ids.Type
	}{
		{"runtime_executable", m.Executable, ids.TypeSource},
		{"multica_profile", m.Profile, ids.TypeSource},
		{"multica_endpoint", m.ServerURL, ids.TypeSource},
		{"multica_workspace", m.WorkspaceID, ids.TypeProject},
		{"multica_assignee", target.AgentID, ids.TypeSource},
		{"multica_runtime", target.RuntimeID, ids.TypeSource},
	}
	bindings := make([]model.SourceBinding, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.value) == "" {
			continue
		}
		binding, err := newDispatchBinding(item.kind, item.value, item.typ)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

func newDispatchBinding(kind, value string, typ ids.Type) (model.SourceBinding, error) {
	alias, err := ids.New(typ)
	if err != nil {
		return model.SourceBinding{}, err
	}
	copy := value
	return model.SourceBinding{Kind: kind, AliasID: alias, Fingerprint: adapter.Fingerprint("multica", kind, value), OpaqueID: &copy}, nil
}

func dispatchTargetFromBindings(execution model.Execution, selected dispatchRoute) (dispatchTarget, error) {
	agentID, hasAgent := dispatchBindingOpaque(execution.SourceBindings, "multica_assignee")
	runtimeID, hasRuntime := dispatchBindingOpaque(execution.SourceBindings, "multica_runtime")
	if !hasAgent || !hasRuntime {
		return dispatchTarget{}, errors.New("prepared dispatch omitted exact assignee or runtime binding")
	}
	return dispatchTarget{AgentID: agentID, RuntimeID: runtimeID, Route: selected}, nil
}

func dispatchBindingOpaque(bindings []model.SourceBinding, kind string) (string, bool) {
	for _, binding := range bindings {
		if binding.Kind == kind && binding.OpaqueID != nil && strings.TrimSpace(*binding.OpaqueID) != "" {
			return *binding.OpaqueID, true
		}
	}
	return "", false
}

func publicDispatchTarget(target dispatchTarget) map[string]any {
	value := map[string]any{"route": target.Route}
	if target.AgentName != "" {
		value["agent_name"] = target.AgentName
	}
	if target.RuntimeName != "" {
		value["runtime_name"] = target.RuntimeName
	}
	return value
}

func dispatchPreparedCapabilities(now time.Time) model.CapabilitySnapshot {
	reason := "Multica issue has not been created or recovered yet"
	return model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "0.1.0", Items: []model.CapabilityItem{
		{Name: "durable_idempotency", Status: model.CapabilitySupported, Source: "multica_api", SemanticsVersion: 1},
		{Name: "events", Status: model.CapabilityUnavailable, Source: "manifest", SemanticsVersion: 1, Reason: &reason},
		{Name: "snapshot", Status: model.CapabilityUnavailable, Source: "manifest", SemanticsVersion: 1, Reason: &reason},
	}}
}

func dispatchStringPointer(value string) *string { return &value }
