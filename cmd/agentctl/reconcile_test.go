package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const reconcileSecret = "RECONCILE_PRIVATE_RESULT"

type reconcileWorld struct {
	path                                                          string
	now                                                           time.Time
	gone, alive, recent, leased, plain, cancelled, recentTerminal model.Execution
	failed, conflicted, preEpoch, multica, multicaDone, codex     model.Execution
	unproven, startUnproven                                       model.Execution
}

func TestReconcilePlanWritesNothingAndSkipsLiveWork(t *testing.T) {
	world := seedReconcileWorld(t)
	stdout, app := reconcileApp(t, world)
	planned := world.run(t, app, stdout)
	assertResultMatchesSchemaShape(t, stdout.String(), "reconcile-plan.schema.json", "orphan", "collect", "multica", "unchanged")
	if planned.Applied || planned.Mode != "plan" || strings.Contains(stdout.String(), reconcileSecret) {
		t.Fatalf("plan wrote or leaked content: %s", stdout.String())
	}
	if !sameIDs(idsOfOrphans(planned), world.gone.ID, world.codex.ID) {
		t.Fatalf("orphans=%v", idsOfOrphans(planned))
	}
	if !sameIDs(idsOfCollect(planned), world.plain.ID, world.cancelled.ID, world.multicaDone.ID) {
		t.Fatalf("collect=%v", idsOfCollect(planned))
	}
	if !sameIDs(idsOfMultica(planned), world.multica.ID) {
		t.Fatalf("multica=%v", idsOfMultica(planned))
	}
	if planned.Unchanged.Alive != 1 || planned.Unchanged.Recent != 2 {
		t.Fatalf("unchanged=%+v", planned.Unchanged)
	}
	reasons := map[string]string{}
	for _, item := range planned.Unproven {
		reasons[item.ID.String()] = item.Reason
	}
	if reasons[world.unproven.ID.String()] != "ownership_unproven" || reasons[world.startUnproven.ID.String()] != "start_unproven" || len(planned.Unproven) != 2 {
		t.Fatalf("unproven=%v", planned.Unproven)
	}
	if app.processCalls[4343] != 1 || app.processCalls[4444] != 0 || app.processCalls[4545] != 0 || app.processCalls[4747] != 0 {
		t.Fatalf("proof calls=%v", app.processCalls)
	}
	assertUntouched(t, world.path, world.gone, world.alive, world.multica, world.plain)

	filtered := world.run(t, app, stdout, "--adapter", "codex")
	if !sameIDs(idsOfOrphans(filtered), world.codex.ID) || filtered.Collect.Count != 0 || filtered.Multica.Count != 0 {
		t.Fatalf("adapter filter=%s", stdout.String())
	}
	labeled := world.run(t, app, stdout, "--label", "review", "--label", "batch")
	if !sameIDs(idsOfOrphans(labeled), world.gone.ID) || labeled.Collect.Count != 0 || labeled.Unchanged.Alive != 0 {
		t.Fatalf("label filter=%s", stdout.String())
	}
	excluded := world.run(t, app, stdout, "--label", "review", "--label", "missing")
	if excluded.Orphan.Count != 0 || excluded.Collect.Count != 0 || excluded.Multica.Count != 0 {
		t.Fatalf("AND label matched too much: %s", stdout.String())
	}
	failures := world.run(t, app, stdout, "--include-failures")
	if !sameIDs(idsOfCollect(failures), world.plain.ID, world.cancelled.ID, world.multicaDone.ID, world.failed.ID, world.conflicted.ID) {
		t.Fatalf("include-failures collect=%v", idsOfCollect(failures))
	}
	if strings.Contains(stdout.String(), reconcileSecret) {
		t.Fatalf("failure plan leaked result content")
	}
}

func TestReconcileApplyIsIdempotentAndDoesNotReadResults(t *testing.T) {
	world := seedReconcileWorld(t)
	stdout, app := reconcileApp(t, world)
	applied := world.run(t, app, stdout, "--apply")
	if !applied.Applied || strings.Contains(stdout.String(), reconcileSecret) {
		t.Fatalf("apply leaked or did not apply: %s", stdout.String())
	}
	if !sameIDs(idsOfOrphans(applied), world.gone.ID, world.codex.ID) {
		t.Fatalf("applied orphans=%v", idsOfOrphans(applied))
	}
	if !sameIDs(idsOfCollect(applied), world.plain.ID, world.cancelled.ID, world.multicaDone.ID) {
		t.Fatalf("applied collect=%v", idsOfCollect(applied))
	}
	journal := openReconcileJournal(t, world.path)
	orphaned, err := journal.GetExecution(context.Background(), world.gone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if orphaned.State != model.StateOrphaned || orphaned.SourceState == nil || *orphaned.SourceState != "owner_lost" {
		t.Fatalf("orphaned=%+v", orphaned)
	}
	outcome, err := journal.GetOutcome(context.Background(), world.gone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Content != nil || outcome.Failure == nil || outcome.Failure.Code != "owner_lost" || outcome.Failure.Kind != "observation" || outcome.NativeExitCode != nil {
		t.Fatalf("owner-lost outcome=%+v", outcome)
	}
	for _, id := range []ids.ExecutionID{world.plain.ID, world.cancelled.ID, world.multicaDone.ID} {
		ack, err := journal.GetAcknowledgement(context.Background(), id)
		if err != nil || ack.Source != store.AcknowledgementBulk {
			t.Fatalf("ack %s err=%v value=%+v", id, err, ack)
		}
	}
	for _, id := range []ids.ExecutionID{world.failed.ID, world.conflicted.ID, world.preEpoch.ID, world.recentTerminal.ID, world.gone.ID} {
		if _, err := journal.GetAcknowledgement(context.Background(), id); err == nil {
			t.Fatalf("%s was collected", id)
		}
	}
	alive, err := journal.GetExecution(context.Background(), world.alive.ID)
	if err != nil || alive.State != model.StateRunning || alive.Revision != world.alive.Revision {
		t.Fatalf("alive changed: %+v err=%v", alive, err)
	}
	waiting, err := journal.GetExecution(context.Background(), world.multica.ID)
	if err != nil || waiting.State != model.StateWaiting || waiting.Revision != world.multica.Revision {
		t.Fatalf("multica changed: %+v err=%v", waiting, err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	again := world.run(t, app, stdout, "--apply")
	if again.Orphan.Count != 0 || again.Collect.Count != 0 || strings.Contains(stdout.String(), reconcileSecret) {
		t.Fatalf("replay was not empty: %s", stdout.String())
	}
}

func TestReconcileReportsMulticaBindingWithoutMutatingIt(t *testing.T) {
	world := seedReconcileWorld(t)
	stdout, app := reconcileApp(t, world)
	planned := world.run(t, app, stdout, "--apply")
	item := planned.Multica.Executions[0]
	if item.ID != world.multica.ID || item.NextAction.Mutates || len(item.Bindings) != 2 {
		t.Fatalf("multica report=%+v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "MUL-ISSUE-EXACT") || !strings.Contains(string(encoded), "MUL-RUN-EXACT") {
		t.Fatalf("exact binding missing: %s", encoded)
	}
	if item.NextAction.Argv[0] != "agentctl" || item.NextAction.Argv[1] != "status" {
		t.Fatalf("next action=%+v", item.NextAction)
	}
	journal := openReconcileJournal(t, world.path)
	defer journal.Close()
	stored, err := journal.GetExecution(context.Background(), world.multica.ID)
	if err != nil || stored.Revision != world.multica.Revision || stored.State != model.StateWaiting {
		t.Fatalf("waiting multica was mutated: %+v err=%v", stored, err)
	}
}

func TestClassifyReconcileSkipsForeignHostWithoutProving(t *testing.T) {
	observed := time.Now().Add(-48 * time.Hour)
	execution := model.Execution{OriginHostID: ids.HostID("other-host"), Authority: model.AuthorityNative, State: model.StateRunning, Observation: model.Observation{ObservedAt: observed}}
	calls := 0
	report := classifyReconcile([]model.Execution{execution}, store.AcknowledgementIndex{}, ids.HostID("this-host"), time.Now(), reconcileOptions{staleAfter: 24 * time.Hour, labels: []string{}}, func(int, time.Time) processProof {
		calls++
		return processProof{Proof: "pid_absent", Gone: true}
	})
	if calls != 0 || report.Orphan.Count != 0 || len(report.Unproven) != 1 || report.Unproven[0].Reason != "foreign_host" {
		t.Fatalf("calls=%d report=%+v", calls, report)
	}
}

func TestParseReconcileRequiresOneModeAndBoundedDurations(t *testing.T) {
	if _, problem := parseReconcile(nil); problem == nil {
		t.Fatal("missing mode accepted")
	}
	if _, problem := parseReconcile([]string{"--plan", "--apply"}); problem == nil {
		t.Fatal("both modes accepted")
	}
	for _, value := range []string{"59s", "721h", "31d", "forever"} {
		if _, problem := parseReconcile([]string{"--plan", "--stale-after", value}); problem == nil {
			t.Fatalf("stale-after %q accepted", value)
		}
	}
	opts, problem := parseReconcile([]string{"--plan", "--stale-after", "24h", "--collect-older-than", "7d", "--include-failures", "--adapter", "cursor", "--label", "review"})
	if problem != nil || opts.staleAfter != 24*time.Hour || opts.collectOlder != 7*24*time.Hour || !opts.includeFailures || opts.adapter != "cursor" || len(opts.labels) != 1 {
		t.Fatalf("options=%+v problem=%v", opts, problem)
	}
	week, err := parseReconcileDuration("7d")
	if err != nil || week != 168*time.Hour {
		t.Fatalf("7d=%s err=%v", week, err)
	}
}

type reconcileTestApp struct {
	*app
	processCalls map[int]int
}

func reconcileApp(t *testing.T, world reconcileWorld) (*bytes.Buffer, *reconcileTestApp) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	base := testApp(&stdout, &stderr)
	base.now = func() time.Time { return world.now }
	wrapped := &reconcileTestApp{app: base, processCalls: map[int]int{}}
	base.processProof = func(pid int, _ time.Time) processProof {
		wrapped.processCalls[pid]++
		switch pid {
		case 4242, 4646:
			return processProof{Proof: "pid_absent", Gone: true}
		case 4343:
			return processProof{Proof: "pid_alive", Alive: true, Present: true}
		case 4848:
			return processProof{Proof: "start_unproven", Present: true}
		default:
			t.Errorf("unexpected process proof for pid %d", pid)
			return processProof{Proof: "start_unproven"}
		}
	}
	return &stdout, wrapped
}

func (w reconcileWorld) run(t *testing.T, app *reconcileTestApp, stdout *bytes.Buffer, extra ...string) reconcileReport {
	t.Helper()
	stdout.Reset()
	args := append([]string{"--output", "json", "--journal", w.path, "reconcile"}, extra...)
	if !containsArg(extra, "--plan") && !containsArg(extra, "--apply") {
		args = append(args, "--plan")
	}
	if code := app.run(context.Background(), args); code != 0 {
		t.Fatalf("%v exit=%d %s", args, code, stdout.String())
	}
	var document struct {
		OK     bool            `json:"ok"`
		Result reconcileReport `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil || !document.OK {
		t.Fatalf("decode %v: %s", err, stdout.String())
	}
	return document.Result
}

func seedReconcileWorld(t *testing.T) reconcileWorld {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	index, err := journal.AcknowledgementIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Now().UTC().Add(2 * time.Second)
	world := reconcileWorld{path: path, now: anchor.Add(8 * 24 * time.Hour)}
	staleAt := world.now.Add(-48 * time.Hour)
	world.gone = mustCreateRunning(t, journal, "cursor", "cursor_session", "4242", staleAt, []string{"review", "batch"})
	world.alive = mustCreateRunning(t, journal, "cursor", "cursor_session", "4343", staleAt, nil)
	world.recent = mustCreateRunning(t, journal, "cursor", "cursor_session", "4444", world.now.Add(-time.Hour), nil)
	world.leased = mustCreateRunning(t, journal, "cursor", "cursor_session", "4545", world.now.Add(-30*time.Hour), nil)
	lease := 48 * 3600
	world.leased.Liveness = model.LivenessAlive
	world.leased.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: world.now.Add(-30 * time.Hour), FreshForSeconds: &lease}
	world.leased.UpdatedAt = world.leased.Observation.ObservedAt
	world.leased, err = journal.UpdateExecution(context.Background(), world.leased, world.leased.Revision)
	if err != nil {
		t.Fatal(err)
	}
	world.codex = mustCreateRunning(t, journal, "codex", "codex_thread", "4646", staleAt, nil)
	world.unproven = mustCreateRunning(t, journal, "cursor", "cursor_session", "not-a-pid", staleAt, nil)
	world.startUnproven = mustCreateRunning(t, journal, "cursor", "cursor_session", "4848", staleAt, nil)
	terminalAt := anchor
	world.plain = mustFinish(t, journal, mustCreateRunning(t, journal, "cursor", "cursor_session", "4747", staleAt, nil), model.StateCompleted, terminalAt, reconcileSecret)
	world.cancelled = mustFinish(t, journal, mustCreateRunning(t, journal, "cursor", "process", "4748", staleAt, nil), model.StateCancelled, terminalAt, "")
	world.failed = mustFinish(t, journal, mustCreateRunning(t, journal, "cursor", "process", "4749", staleAt, nil), model.StateFailed, terminalAt, "")
	world.conflicted = mustFinish(t, journal, mustCreateRunning(t, journal, "cursor", "process", "4750", staleAt, nil), model.StateCompleted, terminalAt, "conflicted")
	world.conflicted.Observation.Integrity = model.IntegrityConflicted
	world.conflicted.UpdatedAt = world.conflicted.UpdatedAt.Add(time.Second)
	world.conflicted, err = journal.UpdateExecution(context.Background(), world.conflicted, world.conflicted.Revision)
	if err != nil {
		t.Fatal(err)
	}
	recentAt := world.now.Add(-time.Hour)
	world.recentTerminal = mustCreateTerminal(t, journal, "cursor", model.StateCompleted, recentAt)
	world.preEpoch = mustCreateTerminal(t, journal, "cursor", model.StateCompleted, index.Epoch.Add(-time.Hour))
	world.multica = mustCreateMultica(t, journal, model.StateWaiting, staleAt, false)
	world.multicaDone = mustCreateMultica(t, journal, model.StateCompleted, terminalAt, true)
	return world
}

func mustCreateRunning(t *testing.T, journal *store.Journal, adapterName, kind, opaque string, observed time.Time, labels []string) model.Execution {
	t.Helper()
	started := observed
	execution := model.Execution{
		Authority: model.AuthorityNative, Adapter: adapterName, Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched,
		State: model.StateRunning, Liveness: model.LivenessUnreachable, Labels: labels, StartedAt: &started,
		SourceBindings: []model.SourceBinding{testSourceBinding(t, kind, ids.TypeSource, opaque)},
		Capabilities:   model.CapabilitySnapshot{NegotiatedAt: observed, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		Observation:    model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityDegraded, ObservedAt: observed},
	}
	created, _, err := journal.CreateExecution(context.Background(), execution, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func mustFinish(t *testing.T, journal *store.Journal, execution model.Execution, state model.State, at time.Time, content string) model.Execution {
	t.Helper()
	if at.Before(execution.CreatedAt) {
		at = execution.CreatedAt.Add(time.Second)
	}
	execution.State = state
	execution.Liveness = model.LivenessExited
	execution.TerminalAt = &at
	execution.UpdatedAt = at
	execution.Observation.ObservedAt = at
	outcome := model.Outcome{
		SchemaVersion: model.SchemaVersion, ExecutionID: execution.ID, Revision: 1, State: state, Availability: model.OutcomeStored,
		RecordedAt: at, Source: execution.Adapter, ResultRef: "agentctl://" + execution.OriginHostID.String() + "/" + execution.ID.String(),
	}
	if content != "" && state == model.StateCompleted {
		sum := sha256.Sum256([]byte(content))
		outcome.Content = &model.OutcomeContent{MediaType: "text/plain", Text: content, Preview: content, Bytes: len(content), SHA256: "sha256:" + hex.EncodeToString(sum[:])}
	} else {
		outcome.Failure = &model.OutcomeFailure{Code: "native_execution_failed", Kind: "unknown", Source: execution.Adapter, Message: "fixture failure"}
	}
	event, canonical, err := syntheticEvent(execution, model.EventTerminal, state, map[string]any{"diagnostic_code": "fixture"}, "test", at)
	if err != nil {
		t.Fatal(err)
	}
	updated, _, _, _, err := journal.CommitTerminalOutcome(context.Background(), execution, execution.Revision, outcome, event, canonical)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func mustCreateTerminal(t *testing.T, journal *store.Journal, adapterName string, state model.State, at time.Time) model.Execution {
	t.Helper()
	execution := model.Execution{
		Authority: model.AuthorityNative, Adapter: adapterName, Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched,
		State: state, Liveness: model.LivenessExited, CreatedAt: at, UpdatedAt: at, TerminalAt: &at,
		SourceBindings: []model.SourceBinding{},
		Capabilities:   model.CapabilitySnapshot{NegotiatedAt: at, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		Observation:    model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityDegraded, ObservedAt: at},
	}
	created, _, err := journal.CreateExecution(context.Background(), execution, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func mustCreateMultica(t *testing.T, journal *store.Journal, state model.State, at time.Time, terminal bool) model.Execution {
	t.Helper()
	execution := model.Execution{
		Authority: model.AuthorityMultica, Adapter: "multica", Mode: model.ModeMultica, Acquisition: model.AcquisitionLaunched,
		State: state, Liveness: model.LivenessUnknown,
		SourceBindings: []model.SourceBinding{
			testSourceBinding(t, "multica_issue", ids.TypeIssue, "MUL-ISSUE-EXACT"),
			testSourceBinding(t, "multica_run", ids.TypeRun, "MUL-RUN-EXACT"),
		},
		Capabilities: model.CapabilitySnapshot{NegotiatedAt: at, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		Observation:  model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityDegraded, ObservedAt: at},
	}
	if terminal {
		execution.CreatedAt, execution.UpdatedAt, execution.TerminalAt = at, at, &at
		execution.Liveness = model.LivenessExited
	}
	created, _, err := journal.CreateExecution(context.Background(), execution, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func testSourceBinding(t *testing.T, kind string, aliasType ids.Type, opaque string) model.SourceBinding {
	t.Helper()
	alias, err := ids.New(aliasType)
	if err != nil {
		t.Fatal(err)
	}
	value := opaque
	return model.SourceBinding{Kind: kind, AliasID: alias, Fingerprint: adapter.Fingerprint(kind, opaque), OpaqueID: &value}
}

func openReconcileJournal(t *testing.T, path string) *store.Journal {
	t.Helper()
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func assertUntouched(t *testing.T, path string, executions ...model.Execution) {
	t.Helper()
	journal := openReconcileJournal(t, path)
	defer journal.Close()
	acks, err := journal.AcknowledgementIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(acks.ByID) != 0 {
		t.Fatalf("plan stored acknowledgements: %+v", acks.ByID)
	}
	for _, want := range executions {
		got, err := journal.GetExecution(context.Background(), want.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != want.Revision || got.State != want.State {
			t.Fatalf("plan changed %s revision %d->%d state %s->%s", want.ID, want.Revision, got.Revision, want.State, got.State)
		}
	}
}

func idsOfOrphans(report reconcileReport) []ids.ExecutionID {
	values := make([]ids.ExecutionID, 0, len(report.Orphan.Executions))
	for _, item := range report.Orphan.Executions {
		values = append(values, item.ID)
	}
	return values
}

func idsOfCollect(report reconcileReport) []ids.ExecutionID {
	values := make([]ids.ExecutionID, 0, len(report.Collect.Executions))
	for _, item := range report.Collect.Executions {
		values = append(values, item.ID)
	}
	return values
}

func idsOfMultica(report reconcileReport) []ids.ExecutionID {
	values := make([]ids.ExecutionID, 0, len(report.Multica.Executions))
	for _, item := range report.Multica.Executions {
		values = append(values, item.ID)
	}
	return values
}

func sameIDs(got []ids.ExecutionID, want ...ids.ExecutionID) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[ids.ExecutionID]int{}
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		seen[id]--
		if seen[id] < 0 {
			return false
		}
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}
