package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestInboxCommandExplainsWithoutReadingResultContent(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	native := `{"type":"result","status":"completed","result":"INBOX_PRIVATE_RESULT"}`
	if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", native}); code != 0 {
		t.Fatalf("run exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "inbox"}); code != 0 {
		t.Fatalf("inbox exit=%d output=%s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), "INBOX_PRIVATE_RESULT") {
		t.Fatalf("inbox leaked result content: %s", stdout.String())
	}
	var document struct {
		Result struct {
			Executions []inboxExecution `json:"executions"`
			Count      int              `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Result.Count != 1 || len(document.Result.Executions) != 1 {
		t.Fatalf("inbox=%s", stdout.String())
	}
	if got := inboxReasonCodes(document.Result.Executions[0].Reasons); !equalStrings(got, []string{"result_unreconciled"}) {
		t.Fatalf("reasons=%v", got)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "text", "--journal", journalPath, "inbox"}); code != 0 {
		t.Fatalf("text inbox exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `why=["result_unreconciled"]`) || strings.Contains(stdout.String(), "INBOX_PRIVATE_RESULT") {
		t.Fatalf("text inbox=%s", stdout.String())
	}
}

func TestInboxCommandKeepsConflictedTerminalVisibleAfterAcknowledgement(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	native := `{"type":"result","status":"completed","result":"CONFLICTED_PRIVATE_RESULT"}`
	if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", native}); code != 0 {
		t.Fatalf("run exit=%d output=%s", code, stdout.String())
	}
	var runDocument struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDocument); err != nil {
		t.Fatal(err)
	}
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := journal.GetExecution(context.Background(), runDocument.Result.ID)
	if err != nil {
		t.Fatal(err)
	}
	execution.Observation.Integrity = model.IntegrityConflicted
	execution.UpdatedAt = execution.UpdatedAt.Add(time.Second)
	execution.Observation.ObservedAt = execution.UpdatedAt
	if _, err := journal.UpdateExecution(context.Background(), execution, execution.Revision); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "result", execution.ID.String()}); code != 13 || !strings.Contains(stdout.String(), `"code":"unknown_state"`) {
		t.Fatalf("conflicted result exit=%d output=%s", code, stdout.String())
	}
	journal, err = store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := journal.AcknowledgeExecution(context.Background(), execution.ID, store.AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "inbox"}); code != 0 {
		t.Fatalf("inbox exit=%d output=%s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), "CONFLICTED_PRIVATE_RESULT") {
		t.Fatalf("inbox leaked conflicted result content: %s", stdout.String())
	}
	var inboxDocument struct {
		Result struct {
			Executions []inboxExecution `json:"executions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &inboxDocument); err != nil {
		t.Fatal(err)
	}
	if len(inboxDocument.Result.Executions) != 1 {
		t.Fatalf("inbox=%s", stdout.String())
	}
	item := inboxDocument.Result.Executions[0]
	if item.WorkHealth != "integrity_conflicted" || item.Unreconciled {
		t.Fatalf("item=%#v", item)
	}
	if got, want := inboxReasonCodes(item.Reasons), []string{"observation_integrity_conflicted"}; !equalStrings(got, want) {
		t.Fatalf("reasons=%v want=%v", got, want)
	}
}

func TestInboxSeparatesWorkFromToolHealth(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	execution := model.Execution{
		State:       model.StateRunning,
		Liveness:    model.LivenessUnreachable,
		CreatedAt:   now.Add(-3 * time.Hour),
		UpdatedAt:   now.Add(-2 * time.Hour),
		Observation: model.Observation{ObservedAt: now.Add(-2 * time.Hour)},
	}
	item, actionable := projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
	if !actionable {
		t.Fatal("stale unreachable work was not actionable")
	}
	if item.WorkHealth != "observation_stale" || item.ToolHealth != "unreachable" {
		t.Fatalf("health projection=%#v", item)
	}
	want := []string{"running_observation_stale", "tool_unreachable"}
	if got := inboxReasonCodes(item.Reasons); !equalStrings(got, want) {
		t.Fatalf("reasons=%v want=%v", got, want)
	}
	if item.Reasons[1].Summary != "the runtime is unreachable; this does not prove the work failed" {
		t.Fatalf("unreachable reason overclaims task failure: %#v", item.Reasons[1])
	}
}

func TestInboxTerminalFailureClearsOnCollection(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	terminalAt := now.Add(-time.Minute)
	execution := model.Execution{
		State:       model.StateFailed,
		Liveness:    model.LivenessExited,
		CreatedAt:   now.Add(-time.Hour),
		UpdatedAt:   terminalAt,
		TerminalAt:  &terminalAt,
		Observation: model.Observation{ObservedAt: terminalAt},
	}
	acks := store.AcknowledgementIndex{Epoch: now.Add(-2 * time.Hour), ByID: map[ids.ExecutionID]store.ExecutionAcknowledgement{}}
	item, actionable := projectInbox(execution, now, time.Hour, acks, output.JSON)
	if !actionable || item.WorkHealth != "failed" || !item.Unreconciled {
		t.Fatalf("uncollected failure=%#v actionable=%v", item, actionable)
	}
	if item.Recovery != nil || len(item.NextActions) != 0 {
		t.Fatalf("ordinary terminal collection received stale-work guidance: %+v", item)
	}
	want := []string{"execution_failed", "result_unreconciled"}
	if got := inboxReasonCodes(item.Reasons); !equalStrings(got, want) {
		t.Fatalf("reasons=%v want=%v", got, want)
	}
	acks.ByID[execution.ID] = store.ExecutionAcknowledgement{ExecutionID: execution.ID, AcknowledgedAt: now, Source: store.AcknowledgementResult}
	if _, actionable := projectInbox(execution, now, time.Hour, acks, output.JSON); actionable {
		t.Fatal("acknowledged terminal failure remained in the inbox")
	}
}

func TestInboxConflictedTerminalRemainsActionableAfterCollection(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	terminalAt := now.Add(-time.Minute)
	execution := model.Execution{
		State: model.StateCompleted, Liveness: model.LivenessExited,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: terminalAt, TerminalAt: &terminalAt,
		Observation: model.Observation{ObservedAt: terminalAt, Integrity: model.IntegrityConflicted},
	}
	acks := store.AcknowledgementIndex{Epoch: now.Add(-2 * time.Hour), ByID: map[ids.ExecutionID]store.ExecutionAcknowledgement{}}

	item, actionable := projectInbox(execution, now, time.Hour, acks, output.JSON)
	if !actionable || item.WorkHealth != "integrity_conflicted" || !item.Unreconciled {
		t.Fatalf("uncollected conflicted terminal=%#v actionable=%v", item, actionable)
	}
	if got, want := inboxReasonCodes(item.Reasons), []string{"observation_integrity_conflicted", "result_unreconciled"}; !equalStrings(got, want) {
		t.Fatalf("uncollected reasons=%v want=%v", got, want)
	}
	if item.Reasons[0].Domain != "integrity" {
		t.Fatalf("integrity reason=%#v", item.Reasons[0])
	}

	acks.ByID[execution.ID] = store.ExecutionAcknowledgement{ExecutionID: execution.ID, AcknowledgedAt: now, Source: store.AcknowledgementResult}
	item, actionable = projectInbox(execution, now, time.Hour, acks, output.JSON)
	if !actionable || item.WorkHealth != "integrity_conflicted" || item.Unreconciled {
		t.Fatalf("acknowledged conflicted terminal=%#v actionable=%v", item, actionable)
	}
	if got, want := inboxReasonCodes(item.Reasons), []string{"observation_integrity_conflicted"}; !equalStrings(got, want) {
		t.Fatalf("acknowledged reasons=%v want=%v", got, want)
	}
}

func TestInboxIntegrityConflictOutranksStalenessAndKeepsToolReason(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	execution := model.Execution{
		State: model.StateRunning, Liveness: model.LivenessUnreachable,
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
		Observation: model.Observation{ObservedAt: now.Add(-2 * time.Hour), Integrity: model.IntegrityConflicted},
	}
	item, actionable := projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
	if !actionable || item.WorkHealth != "integrity_conflicted" || item.ToolHealth != "unreachable" {
		t.Fatalf("conflicted stale work=%#v actionable=%v", item, actionable)
	}
	want := []string{"observation_integrity_conflicted", "running_observation_stale", "tool_unreachable"}
	if got := inboxReasonCodes(item.Reasons); !equalStrings(got, want) {
		t.Fatalf("reasons=%v want=%v", got, want)
	}
}

func TestInboxAttentionDoesNotRequireStaleness(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	execution := model.Execution{State: model.StateAttention, Liveness: model.LivenessBlocked, CreatedAt: now, UpdatedAt: now, Observation: model.Observation{ObservedAt: now}}
	item, actionable := projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
	if !actionable || item.WorkHealth != "attention_required" || item.ToolHealth != "blocked" {
		t.Fatalf("attention=%#v actionable=%v", item, actionable)
	}
	if got := inboxReasonCodes(item.Reasons); !equalStrings(got, []string{"attention_required"}) {
		t.Fatalf("reasons=%v", got)
	}
}

func TestInboxUnreachableWaitingDoesNotInventWorkFailure(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	execution := model.Execution{State: model.StateWaiting, Liveness: model.LivenessUnreachable, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour), Observation: model.Observation{ObservedAt: now.Add(-2 * time.Hour)}}
	item, actionable := projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
	if !actionable || item.WorkHealth != "active" || item.ToolHealth != "unreachable" {
		t.Fatalf("waiting unreachable=%#v actionable=%v", item, actionable)
	}
	if got := inboxReasonCodes(item.Reasons); !equalStrings(got, []string{"tool_unreachable"}) {
		t.Fatalf("reasons=%v", got)
	}
}

func TestParseInboxBoundsStalenessAndLimit(t *testing.T) {
	opts, problem := parseInbox([]string{"--stale-after", "2h", "--limit", "7", "--adapter", "codex", "--label", "review"})
	if problem != nil {
		t.Fatal(problem)
	}
	if opts.staleAfter != 2*time.Hour || opts.limit != 7 || opts.adapter != "codex" || !equalStrings(opts.labels, []string{"review"}) {
		t.Fatalf("options=%#v", opts)
	}
	for _, value := range []string{"59s", "721h", "forever"} {
		if _, problem := parseInbox([]string{"--stale-after", value}); problem == nil {
			t.Fatalf("--stale-after %q accepted", value)
		}
	}
}

func TestInboxNativeRecoveryUsesRecordedOwnerScope(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	execution := staleInboxExecution(now)
	execution.Capabilities.Items = []model.CapabilityItem{{Name: "snapshot", Source: "manifest", SemanticsVersion: 1, Status: model.CapabilityDegraded, Constraints: map[string]any{"scope": "same_process_only", "cross_restart": false}}}
	item, actionable := projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
	if !actionable || item.Recovery == nil || item.Recovery.Route != "owner_process_only" || item.Recovery.Observation != "cached" {
		t.Fatalf("native recovery=%+v", item)
	}
	if item.State != model.StateRunning || item.Liveness != model.LivenessUnreachable {
		t.Fatalf("recovery changed work state: %+v", item)
	}
	if !strings.Contains(strings.Join(item.Recovery.Limitations, " "), "no durable recovery route") {
		t.Fatalf("missing owner limitation: %+v", item.Recovery)
	}
	assertInboxRecoveryReadOnly(t, item.NextActions)
	if len(item.NextActions) != 3 || item.NextActions[2].Argv[1] != "help" || item.NextActions[2].Argv[2] != "run" {
		t.Fatalf("native actions=%+v", item.NextActions)
	}

	// An adapter name alone is not proof of a same-process capability.
	execution.Capabilities.Items = nil
	item, _ = projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
	if item.Recovery.Route != "unverified" || strings.Contains(strings.Join(item.Recovery.Limitations, " "), "no durable recovery route") {
		t.Fatalf("missing capability was inferred: %+v", item.Recovery)
	}
	execution.Capabilities.Items = []model.CapabilityItem{{Name: "snapshot", Source: "manifest", SemanticsVersion: 1, Status: model.CapabilitySupported, Constraints: map[string]any{"cross_restart": false}}}
	item, _ = projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
	if item.Recovery.Route != "unverified" {
		t.Fatalf("cross-restart limitation was inferred to be same-process scope: %+v", item.Recovery)
	}
}

func TestInboxMulticaRecoveryRequiresExactBoundRefreshCapability(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	execution := staleInboxExecution(now)
	execution.Authority, execution.Mode, execution.Adapter = model.AuthorityMultica, model.ModeMultica, "multica"
	issueAlias, err := ids.New(ids.TypeIssue)
	if err != nil {
		t.Fatal(err)
	}
	opaque := "PRIVATE_MULTICA_ISSUE_ID"
	profile, endpoint, workspace := "test-profile", "https://multica.example.test", "test-workspace"
	execution.SourceBindings = []model.SourceBinding{
		{Kind: "multica_issue", AliasID: issueAlias, OpaqueID: &opaque},
		{Kind: "multica_profile", OpaqueID: &profile},
		{Kind: "multica_endpoint", OpaqueID: &endpoint},
		{Kind: "multica_workspace", OpaqueID: &workspace},
	}
	execution.Capabilities.Items = []model.CapabilityItem{{Name: "snapshot", Source: "manifest", SemanticsVersion: 1, Status: model.CapabilitySupported, Constraints: map[string]any{"scope": "bound_issue", "cross_restart": true}}}
	item, actionable := projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
	if !actionable || item.Recovery == nil || item.Recovery.Route != "bound_multica_authority" || len(item.Recovery.AuthorityAliases) != 1 || item.Recovery.AuthorityAliases[0] != issueAlias {
		t.Fatalf("Multica recovery=%+v", item)
	}
	assertInboxRecoveryReadOnly(t, item.NextActions)
	if len(item.NextActions) != 4 || item.NextActions[2].Argv[1] != "supervisor" || item.NextActions[3].Argv[2] != "await" {
		t.Fatalf("Multica actions=%+v", item.NextActions)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), opaque) {
		t.Fatalf("recovery leaked native identity: %s", encoded)
	}
	for _, condition := range []string{"missing_opaque_binding", "snapshot_unavailable", "run_only", "missing_cross_restart", "missing_configuration", "issue_and_run", "unsupported_semantics"} {
		t.Run(condition, func(t *testing.T) {
			candidate := execution
			candidate.SourceBindings = append([]model.SourceBinding{}, execution.SourceBindings...)
			candidate.Capabilities.Items = append([]model.CapabilityItem{}, execution.Capabilities.Items...)
			switch condition {
			case "missing_opaque_binding":
				candidate.SourceBindings[0].OpaqueID = nil
			case "snapshot_unavailable":
				candidate.Capabilities.Items[0].Status = model.CapabilityUnavailable
			case "run_only":
				candidate.SourceBindings[0].Kind = "multica_run"
			case "issue_and_run":
				runID := "PRIVATE_MULTICA_RUN_ID"
				candidate.SourceBindings = append(candidate.SourceBindings, model.SourceBinding{Kind: "multica_run", OpaqueID: &runID})
			case "unsupported_semantics":
				candidate.Capabilities.Items[0].SemanticsVersion = 999
			case "missing_configuration":
				candidate.SourceBindings = candidate.SourceBindings[:1]
			case "missing_cross_restart":
				candidate.Capabilities.Items[0].Constraints = map[string]any{"scope": "bound_issue"}
			}
			projected, _ := projectInbox(candidate, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
			if projected.Recovery.Route != "unverified" || len(projected.NextActions) != 2 {
				t.Fatalf("unverified refresh was advertised: %+v", projected)
			}
		})
	}
	for _, scenario := range []struct {
		name           string
		snapshotStatus model.CapabilityStatus
		eventsStatus   model.CapabilityStatus
		wantRoute      string
		wantActions    int
	}{
		{"snapshot_blocks_events_fallback", model.CapabilitySupported, model.CapabilitySupported, "unverified", 2},
		{"independent_events_route", model.CapabilityUnavailable, model.CapabilitySupported, "bound_multica_authority", 4},
		{"neither_route_usable", model.CapabilityUnavailable, model.CapabilityUnavailable, "unverified", 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			candidate := execution
			runID := "PRIVATE_MULTICA_RUN_ID"
			candidate.SourceBindings = append(append([]model.SourceBinding{}, execution.SourceBindings...), model.SourceBinding{Kind: "multica_run", OpaqueID: &runID})
			candidate.Capabilities.Items = append([]model.CapabilityItem{}, execution.Capabilities.Items...)
			candidate.Capabilities.Items[0].Status = scenario.snapshotStatus
			candidate.Capabilities.Items = append(candidate.Capabilities.Items, model.CapabilityItem{Name: "events", Source: "manifest", SemanticsVersion: 1, Status: scenario.eventsStatus, Constraints: map[string]any{"cross_restart": true}})
			projected, _ := projectInbox(candidate, now, time.Hour, store.AcknowledgementIndex{}, output.JSON)
			if projected.Recovery.Route != scenario.wantRoute || len(projected.NextActions) != scenario.wantActions {
				t.Fatalf("run-source refresh precedence mismatch: %+v", projected)
			}
			assertInboxRecoveryReadOnly(t, projected.NextActions)
			if scenario.snapshotStatus == model.CapabilitySupported && !strings.Contains(strings.Join(projected.Recovery.Limitations, " "), "does not fall back to events") {
				t.Fatalf("missing run-source limitation: %+v", projected.Recovery)
			}
		})
	}
}

func TestInboxStaleRecoveryIsReadOnlyAndInspectionActionsKeepJournal(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	journal, err := store.Open(journalPath, store.Options{Clock: func() time.Time { return now.Add(-2 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	execution := staleInboxExecution(now)
	execution.Capabilities.Items = []model.CapabilityItem{{Name: "snapshot", Source: "manifest", SemanticsVersion: 1, Status: model.CapabilityDegraded, Constraints: map[string]any{"scope": "same_process_only", "cross_restart": false}}}
	created, _, err := journal.CreateExecution(context.Background(), execution, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.now = func() time.Time { return now }
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "inbox"}); code != 0 {
		t.Fatalf("inbox exit=%d output=%s", code, stdout.String())
	}
	var document struct {
		Result struct {
			Executions []inboxExecution `json:"executions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Result.Executions) != 1 || document.Result.Executions[0].Recovery == nil {
		t.Fatalf("inbox=%s", stdout.String())
	}
	for _, action := range document.Result.Executions[0].NextActions[:2] {
		if len(action.Argv) < 5 || action.Argv[1] != "--journal" || action.Argv[2] != journalPath || action.Argv[4] != created.ID.String() {
			t.Fatalf("action lost journal: %+v", action)
		}
		stdout.Reset()
		if code := a.run(context.Background(), action.Argv[1:]); code != 0 {
			t.Fatalf("inspection exit=%d output=%s", code, stdout.String())
		}
		if action.Argv[3] == "status" && !strings.Contains(stdout.String(), created.ID.String()) {
			t.Fatalf("inspection did not target item %s: %s", created.ID, stdout.String())
		}
	}
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("inbox recovery or its inspection actions mutated the journal")
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "text", "--journal", journalPath, "inbox"}); code != 0 {
		t.Fatalf("text inbox exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), ".recovery route=owner_process_only") || !strings.Contains(stdout.String(), ".next argv=") {
		t.Fatalf("text omitted recovery guidance: %s", stdout.String())
	}
}

func TestInboxExplicitJournalDoesNotInspectAmbientSupervisor(t *testing.T) {
	action := output.NextAction{Label: "Inspect supervisor", Argv: []string{"agentctl", "supervisor", "status"}, SideEffectClass: output.ReadOnly, Preconditions: []string{"same state directory"}}
	scoped := scopeInboxAction(action, common{journalPath: "/selected/state/journal.db"})
	if !equalStrings(scoped.Argv, []string{"agentctl", "help", "supervisor"}) || len(scoped.Preconditions) != 0 {
		t.Fatalf("custom journal advertised ambient supervisor: %+v", scoped)
	}
	defaultScope := scopeInboxAction(action, common{})
	if !equalStrings(defaultScope.Argv, action.Argv) {
		t.Fatalf("default supervisor action changed: %+v", defaultScope)
	}
}

func staleInboxExecution(now time.Time) model.Execution {
	id, _ := ids.FromPayload(ids.TypeExecution, 123)
	return model.Execution{
		ID:        ids.ExecutionID(id.String()),
		Authority: model.AuthorityNative, Mode: model.ModeDirect, Adapter: "generic-process", Acquisition: model.AcquisitionLaunched,
		State: model.StateRunning, Liveness: model.LivenessUnreachable,
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
		Capabilities: model.CapabilitySnapshot{NegotiatedAt: now.Add(-3 * time.Hour), AdapterVersion: "test"},
		Observation:  model.Observation{Source: model.ObservationProcess, Integrity: model.IntegrityVerified, ObservedAt: now.Add(-2 * time.Hour)},
	}
}

func assertInboxRecoveryReadOnly(t *testing.T, actions []output.NextAction) {
	t.Helper()
	if len(actions) == 0 {
		t.Fatal("recovery has no inspection actions")
	}
	for _, action := range actions {
		if action.Mutates || action.SideEffectClass != output.ReadOnly {
			t.Fatalf("recovery action mutates: %+v", action)
		}
		if action.Argv[1] != "status" && action.Argv[1] != "events" && action.Argv[1] != "help" && action.Argv[1] != "supervisor" {
			t.Fatalf("recovery invented an operation: %+v", action)
		}
	}
}

func inboxReasonCodes(reasons []inboxReason) []string {
	codes := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		codes = append(codes, reason.Code)
	}
	return codes
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
