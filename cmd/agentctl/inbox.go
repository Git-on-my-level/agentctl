package main

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	agentruntime "github.com/Git-on-my-level/agentctl/internal/runtime"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const (
	defaultInboxStaleAfter = time.Hour
	minimumInboxStaleAfter = time.Minute
	maximumInboxStaleAfter = 30 * 24 * time.Hour
)

type inboxOptions struct {
	limit      int
	staleAfter time.Duration
	adapter    string
	labels     []string
}

type inboxReason struct {
	Code       string   `json:"code"`
	Domain     string   `json:"domain"`
	Summary    string   `json:"summary"`
	AgeSeconds *float64 `json:"age_seconds,omitempty"`
}

type inboxExecution struct {
	ID                    ids.ExecutionID `json:"id"`
	Labels                []string        `json:"labels"`
	Authority             model.Authority `json:"authority"`
	Adapter               string          `json:"adapter"`
	Mode                  model.Mode      `json:"mode"`
	State                 model.State     `json:"state"`
	Liveness              model.Liveness  `json:"liveness"`
	WorkHealth            string          `json:"work_health"`
	ToolHealth            string          `json:"tool_health"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
	ObservationAgeSeconds float64         `json:"observation_age_seconds"`
	Unreconciled          bool            `json:"unreconciled"`
	Reasons               []inboxReason   `json:"reasons"`
	// NextActions is per item because inbox reports many executions at once and
	// a document-level action cannot name which one it applies to.
	NextActions []output.NextAction `json:"next_actions"`
	Recovery    *inboxRecovery      `json:"recovery,omitempty"`
}

// Recovery is guidance from recorded capabilities, not a live ownership probe.
// Typed aliases locate the authority without disclosing native IDs or paths.
type inboxRecovery struct {
	Authority        model.Authority `json:"authority"`
	Observation      string          `json:"observation"`
	Route            string          `json:"route"`
	Summary          string          `json:"summary"`
	Limitations      []string        `json:"limitations"`
	AuthorityAliases []ids.ID        `json:"authority_aliases"`
}

func (a *app) inbox(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	opts, problem := parseInbox(args)
	if problem != nil {
		return problem
	}
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	executions, err := journal.ListExecutions(ctx, false)
	if err != nil {
		return mapStoreError("list inbox executions", err)
	}
	acks, err := journal.AcknowledgementIndex(ctx)
	if err != nil {
		return mapStoreError("list execution acknowledgements", err)
	}
	now := a.now().UTC()
	items := make([]inboxExecution, 0, opts.limit)
	matched := 0
	for i := len(executions) - 1; i >= 0; i-- {
		execution := executions[i]
		if !inboxFilterMatches(execution, opts) {
			continue
		}
		item, actionable := projectInbox(execution, now, opts.staleAfter, acks, renderer.Mode)
		if !actionable {
			continue
		}
		for j := range item.NextActions {
			item.NextActions[j] = scopeInboxAction(item.NextActions[j], c)
		}
		matched++
		if len(items) < opts.limit {
			items = append(items, item)
		}
	}
	lines := make([]output.Line, 0, len(items))
	for _, item := range items {
		codes := make([]string, 0, len(item.Reasons))
		for _, reason := range item.Reasons {
			codes = append(codes, reason.Code)
		}
		fields := []output.Field{
			{Name: "state", Value: item.State},
			{Name: "work", Value: item.WorkHealth},
			{Name: "tool", Value: item.ToolHealth},
			{Name: "why", Value: codes},
			{Name: "observed_ago", Value: time.Duration(item.ObservationAgeSeconds * float64(time.Second)).Round(time.Second)},
		}
		if len(item.Labels) != 0 {
			fields = append(fields, output.Field{Name: "labels", Value: item.Labels})
		}
		lines = append(lines, output.Line{Lead: item.ID.String(), Fields: fields})
		if item.Recovery != nil {
			lines = append(lines, output.Line{Lead: item.ID.String() + ".recovery", Fields: []output.Field{
				{Name: "route", Value: item.Recovery.Route}, {Name: "summary", Value: item.Recovery.Summary},
				{Name: "limitations", Value: item.Recovery.Limitations}, {Name: "authority_aliases", Value: item.Recovery.AuthorityAliases},
			}})
		}
		for _, action := range item.NextActions {
			lines = append(lines, output.Line{Lead: item.ID.String() + ".next", Fields: []output.Field{{Name: "argv", Value: action.Argv}}})
		}
	}
	// count is the returned projection; total is the full actionable set, so a
	// caller can size the backlog without paging to discover it.
	result := map[string]any{
		"executions":          items,
		"count":               len(items),
		"total":               matched,
		"has_more":            matched > len(items),
		"host_local":          true,
		"as_of":               now,
		"stale_after_seconds": opts.staleAfter.Seconds(),
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write inbox", false, err)
	}
	return nil
}

func parseInbox(args []string) (inboxOptions, *output.Error) {
	opts := inboxOptions{limit: 20, staleAfter: defaultInboxStaleAfter}
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return opts, output.NewError(output.CodeUsage, args[i]+" requires a value", false)
		}
		flag, value := args[i], strings.TrimSpace(args[i+1])
		i++
		switch flag {
		case "--limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 200 {
				return opts, output.NewError(output.CodeUsage, "--limit must be between 1 and 200", false)
			}
			opts.limit = limit
		case "--stale-after":
			staleAfter, err := time.ParseDuration(value)
			if err != nil || staleAfter < minimumInboxStaleAfter || staleAfter > maximumInboxStaleAfter {
				return opts, output.NewError(output.CodeUsage, "--stale-after must be a duration between 1m and 720h", false).WithDetail("stale_after", value)
			}
			opts.staleAfter = staleAfter
		case "--adapter":
			if value == "" {
				return opts, output.NewError(output.CodeUsage, "--adapter cannot be empty", false)
			}
			opts.adapter = value
		case "--label":
			if !validRunLabel(value) {
				return opts, output.NewError(output.CodeUsage, "--label must be an exact valid label", false).WithDetail("label", value)
			}
			opts.labels = append(opts.labels, value)
		default:
			return opts, output.NewError(output.CodeUsage, "unknown inbox flag", false).WithDetail("flag", flag)
		}
	}
	return opts, nil
}

func inboxFilterMatches(execution model.Execution, opts inboxOptions) bool {
	if opts.adapter != "" && execution.Adapter != opts.adapter {
		return false
	}
	for _, wanted := range opts.labels {
		if !containsArg(execution.Labels, wanted) {
			return false
		}
	}
	return true
}

func projectInbox(execution model.Execution, now time.Time, staleAfter time.Duration, acks store.AcknowledgementIndex, mode output.Mode) (inboxExecution, bool) {
	observationAge := now.Sub(execution.Observation.ObservedAt)
	if observationAge < 0 {
		observationAge = 0
	}
	unreconciled := acks.Unreconciled(execution)
	reasons := make([]inboxReason, 0, 4)
	workHealth := "active"
	integrityConflicted := execution.Observation.Integrity == model.IntegrityConflicted
	if integrityConflicted {
		reasons = append(reasons, inboxReason{Code: "observation_integrity_conflicted", Domain: "integrity", Summary: "normalized execution evidence conflicts; outcome-dependent commands remain unavailable until the authority is reconciled"})
	}
	actions := []output.NextAction{}
	if execution.State == model.StateAttention {
		workHealth = "attention_required"
		reasons = append(reasons, inboxReason{Code: "attention_required", Domain: "work", Summary: "the execution authority requires a decision or intervention"})
		// Attention is the one inbox reason whose escape is not `result`: the
		// authority decides, and only then does a wait through attention end.
		actions = append(actions, attentionNextActions(mode, execution)...)
	}
	if unreconciled && (execution.State == model.StateFailed || execution.State == model.StateOrphaned) {
		if execution.State == model.StateFailed {
			workHealth = "failed"
			reasons = append(reasons, inboxReason{Code: "execution_failed", Domain: "work", Summary: "the terminal execution failed and its result has not been collected"})
		} else {
			workHealth = "orphaned"
			reasons = append(reasons, inboxReason{Code: "execution_orphaned", Domain: "work", Summary: "the terminal execution was orphaned and its result has not been collected"})
		}
	}
	if unreconciled {
		if workHealth == "active" {
			workHealth = "result_ready"
		}
		reasons = append(reasons, inboxReason{Code: "result_unreconciled", Domain: "collection", Summary: "the terminal result has not been collected with result or await"})
	}
	if execution.State == model.StateRunning && observationAge >= staleAfter {
		age := observationAge.Seconds()
		workHealth = "observation_stale"
		reasons = append(reasons, inboxReason{Code: "running_observation_stale", Domain: "observation", Summary: "running work has no observation within the selected age bound", AgeSeconds: &age})
	}
	if !execution.State.Terminal() && execution.Liveness == model.LivenessUnreachable && observationAge >= staleAfter {
		age := observationAge.Seconds()
		reasons = append(reasons, inboxReason{Code: "tool_unreachable", Domain: "tool", Summary: "the runtime is unreachable; this does not prove the work failed", AgeSeconds: &age})
	}
	if integrityConflicted {
		workHealth = "integrity_conflicted"
	}
	var recovery *inboxRecovery
	if integrityConflicted || (!execution.State.Terminal() && observationAge >= staleAfter && (execution.State == model.StateRunning || execution.Liveness == model.LivenessUnreachable)) {
		var recoveryActions []output.NextAction
		recovery, recoveryActions = inboxRecoveryGuidance(execution, mode)
		for _, candidate := range recoveryActions {
			if !slices.ContainsFunc(actions, func(action output.NextAction) bool { return slices.Equal(action.Argv, candidate.Argv) }) {
				actions = append(actions, candidate)
			}
		}
	}
	labels := append([]string(nil), execution.Labels...)
	if labels == nil {
		labels = []string{}
	}
	item := inboxExecution{
		ID: execution.ID, Labels: labels, Authority: execution.Authority, Adapter: execution.Adapter, Mode: execution.Mode,
		State: execution.State, Liveness: execution.Liveness, WorkHealth: workHealth, ToolHealth: string(execution.Liveness),
		CreatedAt: execution.CreatedAt, UpdatedAt: execution.UpdatedAt, ObservationAgeSeconds: observationAge.Seconds(),
		Unreconciled: unreconciled, Reasons: reasons, NextActions: actions, Recovery: recovery,
	}
	return item, len(reasons) != 0
}

func inboxRecoveryGuidance(execution model.Execution, mode output.Mode) (*inboxRecovery, []output.NextAction) {
	recovery := &inboxRecovery{
		Authority: execution.Authority, Observation: "cached", Route: "unverified",
		Summary:          "inspect the execution authority; the journal does not establish a recovery route",
		Limitations:      []string{"inbox, status, and events read cached evidence; they do not refresh the authority or establish the current owner", "stale or unreachable evidence does not establish failure or completion"},
		AuthorityAliases: []ids.ID{},
	}
	actions := []output.NextAction{
		{Label: "Inspect cached execution and negotiated capabilities", Argv: []string{"agentctl", "status", execution.ID.String(), "--output", string(mode)}, SideEffectClass: output.ReadOnly, Preconditions: []string{}},
		{Label: "Inspect cached execution events", Argv: []string{"agentctl", "events", execution.ID.String(), "--output", string(mode)}, SideEffectClass: output.ReadOnly, Preconditions: []string{}},
	}
	if execution.Observation.Integrity == model.IntegrityConflicted {
		recovery.Limitations = append(recovery.Limitations, "the authority must reconcile conflicting evidence before outcome-dependent commands are available")
	}
	if execution.Authority == model.AuthorityNative {
		for _, capability := range execution.Capabilities.Items {
			if capability.Name == "snapshot" && capability.Constraints["scope"] == "same_process_only" {
				recovery.Route = "owner_process_only"
				recovery.Summary = "inspect the foreground agentctl process in the parent terminal or harness that launched this execution"
				recovery.Limitations = append(recovery.Limitations, "the recorded native snapshot route is same-process only; there is no durable recovery route after that owner exits", "supervisor visibility does not establish native process ownership or cross-restart cancellation support")
				break
			}
		}
		actions = append(actions, output.NextAction{Label: "Review native ownership and lifecycle", Argv: []string{"agentctl", "help", "run"}, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
		return recovery, actions
	}
	if execution.Authority != model.AuthorityMultica {
		return recovery, actions
	}
	boundIssue, boundRun := false, false
	for _, binding := range execution.SourceBindings {
		if binding.Kind != "multica_issue" && binding.Kind != "issue" && binding.Kind != "multica_run" && binding.Kind != "run" {
			continue
		}
		if binding.AliasID.String() != "" {
			recovery.AuthorityAliases = append(recovery.AuthorityAliases, binding.AliasID)
		}
		if binding.OpaqueID != nil && strings.TrimSpace(*binding.OpaqueID) != "" {
			if binding.Kind == "multica_issue" {
				boundIssue = true
			}
			if binding.Kind == "multica_run" {
				boundRun = true
			}
		}
	}
	// Use the same deterministic configuration validation as refresh itself.
	// Old or incomplete rows must not advertise a runnable authority route.
	profile, _ := dispatchBindingOpaque(execution.SourceBindings, "multica_profile")
	endpoint, _ := dispatchBindingOpaque(execution.SourceBindings, "multica_endpoint")
	workspace, _ := dispatchBindingOpaque(execution.SourceBindings, "multica_workspace")
	multicaSpec := agentruntime.AdapterSpec{Name: "multica", Multica: &adapter.MulticaConfig{Profile: profile, Endpoint: endpoint, Workspace: workspace}}
	configurationComplete := multicaSpec.Validate() == nil
	snapshot, snapshotUsable := inboxUsableCapability(execution.Capabilities, "snapshot")
	events, eventsUsable := inboxUsableCapability(execution.Capabilities, "events")
	// Runtime binding reconstruction selects a bound run before an issue.
	// Multica's snapshot accepts only the issue source, and that usage error
	// does not take the runtime's events fallback. Follow the actual precedence.
	snapshotMismatch := boundRun && snapshotUsable
	snapshotRefresh := snapshotUsable && snapshot.Constraints["cross_restart"] == true && snapshot.Constraints["scope"] == "bound_issue" && boundIssue && !boundRun
	eventsRefresh := eventsUsable && events.Constraints["cross_restart"] == true && (boundIssue || boundRun) && !snapshotMismatch
	refreshSupported := configurationComplete && (snapshotRefresh || eventsRefresh)
	if snapshotMismatch {
		recovery.Limitations = append(recovery.Limitations, "the bound run is selected as the refresh source, but the recorded snapshot route requires an issue source; this snapshot error does not fall back to events")
	}
	if boundIssue || boundRun || len(recovery.AuthorityAliases) != 0 {
		recovery.Summary = "inspect the exact bound issue or run in Multica; Multica remains the work authority"
	}
	if refreshSupported {
		recovery.Route = "bound_multica_authority"
		recovery.Limitations = append(recovery.Limitations, "an issue-status snapshot does not prove a run succeeded or its final answer was captured")
		if !execution.State.Terminal() {
			recovery.Summary += "; explicit await or an already configured supervisor can refresh its recorded authority bindings"
			recovery.Limitations = append(recovery.Limitations, "await refreshes the journal and may acknowledge terminal collection; supervisor status only inspects the service and does not start it or refresh this item")
			actions = append(actions,
				output.NextAction{Label: "Inspect the configured refresh supervisor", Argv: []string{"agentctl", "supervisor", "status", "--output", string(mode)}, SideEffectClass: output.ReadOnly, Preconditions: []string{"the supervisor serves the same state directory as this journal"}},
				output.NextAction{Label: "Review explicit authority refresh and collection semantics", Argv: []string{"agentctl", "help", "await"}, SideEffectClass: output.ReadOnly, Preconditions: []string{}},
			)
		}
	}
	return recovery, actions
}

// Match the runtime's first negotiated capability and semantics version;
// a later duplicate cannot make an unavailable route usable.
func inboxUsableCapability(snapshot model.CapabilitySnapshot, name string) (model.CapabilityItem, bool) {
	for _, capability := range snapshot.Items {
		if capability.Name == name {
			return capability, capability.SemanticsVersion == adapter.SemanticsVersion && (capability.Status == model.CapabilitySupported || capability.Status == model.CapabilityDegraded)
		}
	}
	return model.CapabilityItem{}, false
}

// Execution inspection must keep the journal/profile selected by this inbox.
// Supervisor status is state-directory scoped; an explicit journal requires
// discovery because --journal does not select a supervisor socket.
func scopeInboxAction(action output.NextAction, c common) output.NextAction {
	argv := action.Argv
	if len(argv) >= 3 && argv[0] == "agentctl" && argv[1] == "supervisor" && argv[2] == "status" && c.journalPath != "" {
		// The selected journal may belong to a different state directory; no
		// supervisor socket identity was established by this cached projection.
		action.Argv = []string{"agentctl", "help", "supervisor"}
		action.Label = "Discover supervisor scope before inspection"
		action.Preconditions = []string{}
		return action
	}
	if len(argv) < 2 || argv[0] != "agentctl" || (argv[1] != "status" && argv[1] != "events" && argv[1] != "await" && argv[1] != "result") {
		return action
	}
	scoped := []string{"agentctl"}
	for _, flag := range []struct{ name, value string }{
		{"--profile", c.profile}, {"--context-file", c.contextFile},
		{"--config", c.configPath}, {"--config-bundle", c.configBundle}, {"--journal", c.journalPath},
	} {
		if flag.value != "" {
			scoped = append(scoped, flag.name, flag.value)
		}
	}
	action.Argv = append(scoped, argv[1:]...)
	return action
}
