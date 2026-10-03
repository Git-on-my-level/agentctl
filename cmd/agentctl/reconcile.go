package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	reconcileOwnerLost           = "owner_lost"
)

type reconcileOptions struct {
	plan, apply, includeFailures   bool
	planDigest                     string
	staleAfter, collectOlder       time.Duration
	staleAfterRaw, collectOlderRaw string
	adapter                        string
	labels                         []string
}

type reconcileReport struct {
	SchemaVersion int       `json:"schema_version"`
	Mode          string    `json:"mode"`
	Applied       bool      `json:"applied"`
	AsOf          time.Time `json:"as_of"`
	PlanDigest    string    `json:"plan_digest"`
	// JournalHostMatch reports that candidate selection compared each
	// execution's origin_host_id with the host id stored in this journal.
	// That id is not a machine fingerprint: a copied or restored journal
	// still matches.
	JournalHostMatch        bool                `json:"journal_host_match"`
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
	report.PlanDigest = reconcilePlanDigest(report, opts)
	if opts.apply {
		if report.PlanDigest != opts.planDigest {
			return output.NewError(output.CodeConflict, "reconcile plan changed; review the current plan and pass its plan_digest", false).WithDetail("plan_digest", report.PlanDigest).WithDetail("expected_plan_digest", opts.planDigest)
		}
		report.Applied = true
		report.Mode = "apply"
		if problem := a.applyReconcile(ctx, journal, host, now, opts, &report); problem != nil {
			return problem
		}
	}
	lines := []output.Line{{Lead: "reconcile", Fields: []output.Field{
		{Name: "mode", Value: report.Mode},
		{Name: "plan_digest", Value: report.PlanDigest},
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
		{Code: "multica_not_mutated", Message: "Multica issue state is not changed; local collection stamps are written"},
		{Code: "journal_host_match", Message: "journal_host_match compares origin_host_id with the host id stored in this journal; it is not a machine fingerprint, so a copied or restored journal still matches"},
	}
	actions := []output.NextAction{}
	if opts.plan {
		actions = append(actions, output.NextAction{
			Label:           "Apply this reconciliation after reviewing the ids",
			Argv:            reconcileArgv(opts, report.PlanDigest),
			Mutates:         true,
			SideEffectClass: output.LocalOperationalWrite,
			Preconditions:   []string{"apply requires this plan_digest and writes nothing if the recomputed candidate ids, actions, and proofs differ", "each row is proved again immediately before it is written", "apply does not read result content", "Multica issue state is not changed; local collection stamps are written"},
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
		case "--plan-digest":
			if i+1 >= len(args) {
				return opts, output.NewError(output.CodeUsage, "--plan-digest requires the digest returned by --plan", false)
			}
			i++
			opts.planDigest = strings.TrimSpace(args[i])
			if opts.planDigest == "" {
				return opts, output.NewError(output.CodeUsage, "--plan-digest requires the digest returned by --plan", false)
			}
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
	if opts.plan && opts.planDigest != "" {
		return opts, output.NewError(output.CodeUsage, "--plan-digest is only valid with --apply", false)
	}
	if opts.apply && opts.planDigest == "" {
		return opts, output.NewError(output.CodeUsage, "reconcile --apply requires --plan-digest from the reviewed plan", false)
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

func classifyReconcile(executions []model.Execution, acks store.AcknowledgementIndex, host ids.HostID, now time.Time, opts reconcileOptions, prove func(processIdentity) processProof) reconcileReport {
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
		identity, ok := recordedLaunch(execution)
		if !ok {
			report.Unproven = append(report.Unproven, reconcileUnproven{ID: execution.ID, Reason: "ownership_unproven"})
			continue
		}
		proof := prove(identity)
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
		SchemaVersion: 1, Mode: mode, AsOf: now, JournalHostMatch: true,
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
		identity, ok := recordedLaunch(current)
		if !ok {
			continue
		}
		proof := a.proveProcess(identity)
		if !proof.Gone {
			continue
		}
		if a.forceStaleRevision != nil && a.forceStaleRevision(current.ID) && current.Revision > 0 {
			current.Revision--
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

func reconcileArgv(opts reconcileOptions, planDigest string) []string {
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
	return append(argv, "--apply", "--plan-digest", planDigest)
}

type reconcileDigestCandidate struct {
	ID                string `json:"id"`
	Action            string `json:"action"`
	Proof             string `json:"proof,omitempty"`
	PID               int    `json:"pid,omitempty"`
	RecordedStartedAt string `json:"recorded_started_at,omitempty"`
}

func reconcilePlanDigest(report reconcileReport, opts reconcileOptions) string {
	candidates := make([]reconcileDigestCandidate, 0, len(report.Orphan.Executions)+len(report.Collect.Executions))
	for _, item := range report.Orphan.Executions {
		candidate := reconcileDigestCandidate{ID: item.ID.String(), Action: "orphan", Proof: item.Evidence.Proof, PID: item.Evidence.PID}
		if !item.Evidence.RecordedStartedAt.IsZero() {
			candidate.RecordedStartedAt = item.Evidence.RecordedStartedAt.UTC().Format(time.RFC3339Nano)
		}
		candidates = append(candidates, candidate)
	}
	for _, item := range report.Collect.Executions {
		candidates = append(candidates, reconcileDigestCandidate{ID: item.ID.String(), Action: "collect"})
	}
	slices.SortFunc(candidates, func(a, b reconcileDigestCandidate) int {
		if cmp := strings.Compare(a.ID, b.ID); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Action, b.Action)
	})
	labels := append([]string(nil), opts.labels...)
	if labels == nil {
		labels = []string{}
	}
	raw, err := json.Marshal(struct {
		SchemaVersion           int                        `json:"schema_version"`
		StaleAfterSeconds       int64                      `json:"stale_after_seconds"`
		CollectOlderThanSeconds int64                      `json:"collect_older_than_seconds"`
		IncludeFailures         bool                       `json:"include_failures"`
		Adapter                 string                     `json:"adapter"`
		Labels                  []string                   `json:"labels"`
		Candidates              []reconcileDigestCandidate `json:"candidates"`
	}{1, int64(opts.staleAfter / time.Second), int64(opts.collectOlder / time.Second), opts.includeFailures, opts.adapter, labels, candidates})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
