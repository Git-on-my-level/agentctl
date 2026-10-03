package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const (
	defaultReconcileStaleAfter   = 24 * time.Hour
	defaultReconcileCollectAfter = 7 * 24 * time.Hour
	minimumReconcileAge          = time.Minute
	maximumReconcileStaleAfter   = 30 * 24 * time.Hour
	maximumReconcileCollectAfter = 365 * 24 * time.Hour
	maximumLaunchPID             = 4194303
	reconcileOwnerLost           = "owner_lost"
)

type reconcileOptions struct {
	plan, apply, includeFailures   bool
	staleAfter, collectOlder       time.Duration
	staleAfterRaw, collectOlderRaw string
	adapter                        string
	labels                         []string
}

type reconcileReport struct {
	SchemaVersion           int                 `json:"schema_version"`
	Mode                    string              `json:"mode"`
	Applied                 bool                `json:"applied"`
	AsOf                    time.Time           `json:"as_of"`
	HostLocal               bool                `json:"host_local"`
	StaleAfterSeconds       float64             `json:"stale_after_seconds"`
	CollectOlderThanSeconds float64             `json:"collect_older_than_seconds"`
	IncludeFailures         bool                `json:"include_failures"`
	Adapter                 string              `json:"adapter"`
	Labels                  []string            `json:"labels"`
	Orphan                  reconcileOrphanSet  `json:"orphan"`
	Collect                 reconcileCollectSet `json:"collect"`
	Multica                 reconcileMulticaSet `json:"multica"`
	Unchanged               reconcileUnchanged  `json:"unchanged"`
	Unproven                []reconcileUnproven `json:"unproven"`
}

type reconcileOrphanSet struct {
	Count      int                   `json:"count"`
	Executions []reconcileOrphanItem `json:"executions"`
}

type reconcileOrphanItem struct {
	ID                    ids.ExecutionID `json:"id"`
	Adapter               string          `json:"adapter"`
	State                 model.State     `json:"state"`
	Liveness              model.Liveness  `json:"liveness"`
	Reason                string          `json:"reason"`
	Evidence              processProof    `json:"evidence"`
	ObservationAgeSeconds float64         `json:"observation_age_seconds"`
}

type reconcileCollectSet struct {
	Count      int                    `json:"count"`
	Executions []reconcileCollectItem `json:"executions"`
}

type reconcileCollectItem struct {
	ID         ids.ExecutionID `json:"id"`
	Adapter    string          `json:"adapter"`
	Authority  model.Authority `json:"authority"`
	State      model.State     `json:"state"`
	TerminalAt time.Time       `json:"terminal_at"`
	Reason     string          `json:"reason"`
}

type reconcileMulticaSet struct {
	Count      int                    `json:"count"`
	Executions []reconcileMulticaItem `json:"executions"`
}

type reconcileMulticaItem struct {
	ID         ids.ExecutionID    `json:"id"`
	Adapter    string             `json:"adapter"`
	State      model.State        `json:"state"`
	Liveness   model.Liveness     `json:"liveness"`
	Bindings   []reconcileBinding `json:"bindings"`
	NextAction output.NextAction  `json:"next_action"`
}

type reconcileBinding struct {
	Kind     string `json:"kind"`
	AliasID  string `json:"alias_id"`
	OpaqueID string `json:"opaque_id,omitempty"`
}

type reconcileUnchanged struct {
	Alive  int `json:"alive"`
	Recent int `json:"recent"`
}

type reconcileUnproven struct {
	ID     ids.ExecutionID `json:"id"`
	Reason string          `json:"reason"`
}

func (a *app) reconcile(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	opts, problem := parseReconcile(args)
	if problem != nil {
		return problem
	}
	var journal *store.Journal
	if opts.plan {
		journal, problem = a.openRead(c)
	} else {
		journal, problem = a.openWrite(c)
	}
	if problem != nil {
		return problem
	}
	defer journal.Close()
	host, err := journal.HostID(ctx)
	if err != nil {
		return mapStoreError("read journal host", err)
	}
	executions, err := journal.ListExecutions(ctx, false)
	if err != nil {
		return mapStoreError("list reconcile executions", err)
	}
	acks, err := journal.AcknowledgementIndex(ctx)
	if err != nil {
		return mapStoreError("list execution acknowledgements", err)
	}
	now := a.now().UTC()
	report := classifyReconcile(executions, acks, host, now, opts, a.proveProcess)
	if opts.apply {
		report.Applied = true
		report.Mode = "apply"
		if problem := a.applyReconcile(ctx, journal, host, now, opts, &report); problem != nil {
			return problem
		}
	}
	lines := []output.Line{{Lead: "reconcile", Fields: []output.Field{
		{Name: "mode", Value: report.Mode},
		{Name: "orphan", Value: report.Orphan.Count},
		{Name: "collect", Value: report.Collect.Count},
		{Name: "multica", Value: report.Multica.Count},
		{Name: "unproven", Value: len(report.Unproven)},
	}}}
	for _, item := range report.Orphan.Executions {
		lines = append(lines, output.Line{Lead: item.ID.String(), Fields: []output.Field{{Name: "action", Value: "orphan"}, {Name: "state", Value: item.State}, {Name: "proof", Value: item.Evidence.Proof}, {Name: "pid", Value: item.Evidence.PID}}})
	}
	for _, item := range report.Collect.Executions {
		lines = append(lines, output.Line{Lead: item.ID.String(), Fields: []output.Field{{Name: "action", Value: "collect"}, {Name: "state", Value: item.State}, {Name: "reason", Value: item.Reason}}})
	}
	for _, item := range report.Multica.Executions {
		lines = append(lines, output.Line{Lead: item.ID.String(), Fields: []output.Field{{Name: "action", Value: "report"}, {Name: "state", Value: item.State}, {Name: "bindings", Value: item.Bindings}, {Name: "next", Value: item.NextAction.Argv}}})
	}
	warnings := []output.Warning{
		{Code: "outcome_unknown", Message: "orphaned means the recorded owner is gone and the outcome was not recovered; it is not success or failure"},
		{Code: "multica_not_mutated", Message: "nonterminal Multica executions are reported with their journaled issue binding and are not changed"},
	}
	actions := []output.NextAction{}
	if opts.plan {
		actions = append(actions, output.NextAction{
			Label:           "Apply this reconciliation after reviewing the ids",
			Argv:            reconcileArgv(opts, "apply"),
			Mutates:         true,
			SideEffectClass: output.LocalOperationalWrite,
			Preconditions:   []string{"the same filters select the same executions", "apply is idempotent and does not read result content", "Multica issue state is not changed"},
		})
	}
	if err := renderer.Success(output.Success{Result: report, Lines: lines, Warnings: warnings, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write reconcile", false, err)
	}
	return nil
}

func parseReconcile(args []string) (reconcileOptions, *output.Error) {
	opts := reconcileOptions{
		staleAfter: defaultReconcileStaleAfter, collectOlder: defaultReconcileCollectAfter,
		staleAfterRaw: "24h", collectOlderRaw: "168h", labels: []string{},
	}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "--plan":
			opts.plan = true
		case "--apply":
			opts.apply = true
		case "--include-failures":
			opts.includeFailures = true
		case "--stale-after", "--collect-older-than", "--adapter", "--label":
			if i+1 >= len(args) {
				return opts, output.NewError(output.CodeUsage, flag+" requires a value", false)
			}
			i++
			value := strings.TrimSpace(args[i])
			switch flag {
			case "--stale-after":
				parsed, err := parseReconcileDuration(value)
				if err != nil || parsed < minimumReconcileAge || parsed > maximumReconcileStaleAfter {
					return opts, output.NewError(output.CodeUsage, "--stale-after must be a duration from 1m through 720h", false).WithDetail("stale_after", value)
				}
				opts.staleAfter, opts.staleAfterRaw = parsed, value
			case "--collect-older-than":
				parsed, err := parseReconcileDuration(value)
				if err != nil || parsed < minimumReconcileAge || parsed > maximumReconcileCollectAfter {
					return opts, output.NewError(output.CodeUsage, "--collect-older-than must be a duration from 1m through 8760h", false).WithDetail("collect_older_than", value)
				}
				opts.collectOlder, opts.collectOlderRaw = parsed, value
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
			}
		default:
			return opts, output.NewError(output.CodeUsage, "unknown reconcile flag", false).WithDetail("flag", flag)
		}
	}
	if opts.plan == opts.apply {
		return opts, output.NewError(output.CodeUsage, "reconcile requires exactly one of --plan or --apply", false)
	}
	return opts, nil
}

func parseReconcileDuration(value string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(value, "d"); ok && !strings.Contains(days, "d") {
		count, err := strconv.Atoi(days)
		if err != nil || count < 0 {
			return 0, fmt.Errorf("invalid days")
		}
		return time.Duration(count) * 24 * time.Hour, nil
	}
	return time.ParseDuration(value)
}

func classifyReconcile(executions []model.Execution, acks store.AcknowledgementIndex, host ids.HostID, now time.Time, opts reconcileOptions, prove func(int, time.Time) processProof) reconcileReport {
	report := newReconcileReport(now, opts)
	for _, execution := range executions {
		if !reconcileFilterMatches(execution, opts) {
			continue
		}
		if execution.Authority == model.AuthorityMultica {
			if !execution.State.Terminal() && observationAge(execution, now) >= opts.staleAfter {
				report.Multica.Executions = append(report.Multica.Executions, multicaReport(execution))
			}
		}
		if execution.State.Terminal() {
			if reconcileCollectable(execution, acks, now, opts) {
				report.Collect.Executions = append(report.Collect.Executions, reconcileCollectItem{
					ID: execution.ID, Adapter: execution.Adapter, Authority: execution.Authority, State: execution.State, TerminalAt: execution.TerminalAt.UTC(), Reason: store.AcknowledgementBulk,
				})
			}
			continue
		}
		if execution.Authority != model.AuthorityNative {
			continue
		}
		if execution.OriginHostID != host {
			report.Unproven = append(report.Unproven, reconcileUnproven{ID: execution.ID, Reason: "foreign_host"})
			continue
		}
		age := observationAge(execution, now)
		if age < opts.staleAfter || runnerLeaseActive(execution, now) {
			report.Unchanged.Recent++
			continue
		}
		pid, ok := launchPID(execution)
		if !ok {
			report.Unproven = append(report.Unproven, reconcileUnproven{ID: execution.ID, Reason: "ownership_unproven"})
			continue
		}
		var recorded time.Time
		if execution.StartedAt != nil {
			recorded = *execution.StartedAt
		}
		proof := prove(pid, recorded)
		proof.PID = pid
		if !recorded.IsZero() {
			proof.RecordedStartedAt = recorded.UTC()
		}
		switch {
		case proof.Alive:
			report.Unchanged.Alive++
		case proof.Gone:
			report.Orphan.Executions = append(report.Orphan.Executions, reconcileOrphanItem{
				ID: execution.ID, Adapter: execution.Adapter, State: execution.State, Liveness: execution.Liveness,
				Reason: reconcileOwnerLost, Evidence: proof, ObservationAgeSeconds: age.Seconds(),
			})
		default:
			report.Unproven = append(report.Unproven, reconcileUnproven{ID: execution.ID, Reason: proof.Proof})
		}
	}
	sortReconcile(&report)
	return report
}

func newReconcileReport(now time.Time, opts reconcileOptions) reconcileReport {
	mode := "apply"
	if opts.plan {
		mode = "plan"
	}
	labels := append([]string(nil), opts.labels...)
	if labels == nil {
		labels = []string{}
	}
	return reconcileReport{
		SchemaVersion: 1, Mode: mode, AsOf: now, HostLocal: true,
		StaleAfterSeconds: opts.staleAfter.Seconds(), CollectOlderThanSeconds: opts.collectOlder.Seconds(),
		IncludeFailures: opts.includeFailures, Adapter: opts.adapter, Labels: labels,
		Orphan:   reconcileOrphanSet{Executions: []reconcileOrphanItem{}},
		Collect:  reconcileCollectSet{Executions: []reconcileCollectItem{}},
		Multica:  reconcileMulticaSet{Executions: []reconcileMulticaItem{}},
		Unproven: []reconcileUnproven{},
	}
}

func sortReconcile(report *reconcileReport) {
	slices.SortFunc(report.Orphan.Executions, func(a, b reconcileOrphanItem) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	slices.SortFunc(report.Collect.Executions, func(a, b reconcileCollectItem) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	slices.SortFunc(report.Multica.Executions, func(a, b reconcileMulticaItem) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	slices.SortFunc(report.Unproven, func(a, b reconcileUnproven) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	report.Orphan.Count = len(report.Orphan.Executions)
	report.Collect.Count = len(report.Collect.Executions)
	report.Multica.Count = len(report.Multica.Executions)
}

func reconcileFilterMatches(execution model.Execution, opts reconcileOptions) bool {
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

func reconcileCollectable(execution model.Execution, acks store.AcknowledgementIndex, now time.Time, opts reconcileOptions) bool {
	if !acks.Unreconciled(execution) || execution.TerminalAt == nil {
		return false
	}
	if !execution.TerminalAt.Before(now.Add(-opts.collectOlder)) {
		return false
	}
	if execution.Observation.Integrity == model.IntegrityConflicted || execution.State == model.StateFailed || execution.State == model.StateOrphaned {
		return opts.includeFailures
	}
	return execution.State == model.StateCompleted || execution.State == model.StateCancelled
}

func observationAge(execution model.Execution, now time.Time) time.Duration {
	age := now.Sub(execution.Observation.ObservedAt)
	if age < 0 {
		return 0
	}
	return age
}

func runnerLeaseActive(execution model.Execution, now time.Time) bool {
	if execution.Authority != model.AuthorityNative || execution.State.Terminal() {
		return false
	}
	if execution.Liveness != model.LivenessAlive && execution.Liveness != model.LivenessBlocked {
		return false
	}
	if execution.Observation.Source != model.ObservationNativeStream || execution.Observation.Integrity != model.IntegrityVerified {
		return false
	}
	if execution.Observation.FreshForSeconds == nil || *execution.Observation.FreshForSeconds <= 0 {
		return false
	}
	expires := execution.Observation.ObservedAt.Add(time.Duration(*execution.Observation.FreshForSeconds) * time.Second)
	return !now.After(expires)
}

func launchPID(execution model.Execution) (int, bool) {
	for _, binding := range execution.SourceBindings {
		if !launchProcessKind(binding.Kind) || binding.OpaqueID == nil {
			continue
		}
		raw := strings.TrimSpace(*binding.OpaqueID)
		pid, err := strconv.Atoi(raw)
		if err != nil || pid <= 0 || pid > maximumLaunchPID || strconv.Itoa(pid) != raw {
			continue
		}
		return pid, true
	}
	return 0, false
}

func launchProcessKind(kind string) bool {
	switch kind {
	case "process", "codex_thread", "cursor_session", "claude_session", "omp_session", "zcode_session", "devin_session":
		return true
	default:
		return false
	}
}

func multicaReport(execution model.Execution) reconcileMulticaItem {
	bindings := []reconcileBinding{}
	var issueID string
	for _, binding := range execution.SourceBindings {
		switch binding.Kind {
		case "multica_issue", "issue", "multica_run", "run":
			item := reconcileBinding{Kind: binding.Kind, AliasID: binding.AliasID.String()}
			if binding.OpaqueID != nil {
				item.OpaqueID = strings.TrimSpace(*binding.OpaqueID)
			}
			if item.OpaqueID != "" && (binding.Kind == "multica_issue" || binding.Kind == "issue") && issueID == "" {
				issueID = item.OpaqueID
			}
			bindings = append(bindings, item)
		}
	}
	label := "inspect the bound Multica issue outside agentctl; reconcile does not change Multica"
	if issueID != "" {
		label = "open Multica issue " + issueID + "; reconcile does not change it"
	} else if len(bindings) == 0 {
		label = "the journal has no Multica issue or run binding; reconcile does not change Multica"
	}
	return reconcileMulticaItem{
		ID: execution.ID, Adapter: execution.Adapter, State: execution.State, Liveness: execution.Liveness, Bindings: bindings,
		NextAction: output.NextAction{
			Label: label, Argv: []string{"agentctl", "status", execution.ID.String()}, Mutates: false, SideEffectClass: output.ReadOnly,
			Preconditions: []string{"Multica remains the authority for the bound issue or run", "reconcile does not call Multica"},
		},
	}
}

func (a *app) applyReconcile(ctx context.Context, journal *store.Journal, host ids.HostID, now time.Time, opts reconcileOptions, report *reconcileReport) *output.Error {
	orphans := make([]reconcileOrphanItem, 0, len(report.Orphan.Executions))
	for _, item := range report.Orphan.Executions {
		current, err := journal.GetExecution(ctx, item.ID)
		if err != nil {
			return mapStoreError("reread execution before orphan", err)
		}
		if current.State.Terminal() || current.OriginHostID != host || !reconcileFilterMatches(current, opts) {
			continue
		}
		age := observationAge(current, now)
		if age < opts.staleAfter || runnerLeaseActive(current, now) {
			continue
		}
		pid, ok := launchPID(current)
		if !ok {
			continue
		}
		var recorded time.Time
		if current.StartedAt != nil {
			recorded = *current.StartedAt
		}
		proof := a.proveProcess(pid, recorded)
		if !proof.Gone {
			continue
		}
		proof.PID = pid
		if !recorded.IsZero() {
			proof.RecordedStartedAt = recorded.UTC()
		}
		if _, err := commitOwnerLost(ctx, journal, current, proof, age, now); err != nil {
			if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrTerminalConflict) {
				continue
			}
			return mapStoreError("record owner lost", err)
		}
		item.Evidence = proof
		item.ObservationAgeSeconds = age.Seconds()
		orphans = append(orphans, item)
	}
	report.Orphan.Executions = orphans
	report.Orphan.Count = len(orphans)
	acks, err := journal.AcknowledgementIndex(ctx)
	if err != nil {
		return mapStoreError("reread acknowledgements before collection", err)
	}
	collected := make([]reconcileCollectItem, 0, len(report.Collect.Executions))
	for _, item := range report.Collect.Executions {
		current, err := journal.GetExecution(ctx, item.ID)
		if err != nil {
			return mapStoreError("reread execution before collection", err)
		}
		if !reconcileCollectable(current, acks, now, opts) {
			continue
		}
		_, reused, err := journal.AcknowledgeExecution(ctx, item.ID, store.AcknowledgementBulk)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return mapStoreError("acknowledge reconciled execution", err)
		}
		if reused {
			continue
		}
		acks.ByID[item.ID] = store.ExecutionAcknowledgement{ExecutionID: item.ID, Source: store.AcknowledgementBulk}
		collected = append(collected, item)
	}
	report.Collect.Executions = collected
	report.Collect.Count = len(collected)
	return nil
}

func commitOwnerLost(ctx context.Context, journal *store.Journal, execution model.Execution, proof processProof, age time.Duration, now time.Time) (model.Execution, error) {
	sourceState := reconcileOwnerLost
	execution.State = model.StateOrphaned
	execution.Liveness = model.LivenessUnreachable
	execution.SourceState = &sourceState
	execution.TerminalAt = &now
	execution.UpdatedAt = now
	execution.Observation = model.Observation{Source: model.ObservationReconciled, Integrity: model.IntegrityDegraded, ObservedAt: now}
	outcome := model.Outcome{
		SchemaVersion: model.SchemaVersion, ExecutionID: execution.ID, Revision: 1, State: model.StateOrphaned,
		Availability: model.OutcomeStored, RecordedAt: now, Source: execution.Adapter,
		ResultRef: fmt.Sprintf("agentctl://%s/%s", execution.OriginHostID, execution.ID),
		Failure:   &model.OutcomeFailure{Code: reconcileOwnerLost, Kind: "observation", Source: execution.Adapter, Message: "owning process is gone; outcome unknown"},
	}
	payload := map[string]any{
		"diagnostic_code": reconcileOwnerLost, "reason": reconcileOwnerLost, "proof": proof.Proof, "pid": proof.PID,
		"observation_age_seconds": int(age.Seconds()),
	}
	if !proof.RecordedStartedAt.IsZero() {
		payload["recorded_started_at"] = proof.RecordedStartedAt.Format(time.RFC3339Nano)
	}
	event, canonical, err := syntheticEvent(execution, model.EventTerminal, execution.State, payload, "reconcile", now)
	if err != nil {
		return model.Execution{}, err
	}
	updated, _, _, _, err := journal.CommitTerminalOutcome(ctx, execution, execution.Revision, outcome, event, canonical)
	return updated, err
}

func reconcileArgv(opts reconcileOptions, mode string) []string {
	argv := []string{"agentctl", "reconcile", "--stale-after", opts.staleAfterRaw, "--collect-older-than", opts.collectOlderRaw}
	if opts.adapter != "" {
		argv = append(argv, "--adapter", opts.adapter)
	}
	for _, label := range opts.labels {
		argv = append(argv, "--label", label)
	}
	if opts.includeFailures {
		argv = append(argv, "--include-failures")
	}
	return append(argv, "--"+mode)
}
