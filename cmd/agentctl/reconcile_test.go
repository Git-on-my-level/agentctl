package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strconv"
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
	assertResultMatchesSchemaShape(t, stdout.String(), "reconcile-plan.schema.json", "orphan", "legacy_orphan", "collect", "multica", "unchanged")
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
	if planned.IncludeLegacyUnproven || planned.LegacyOrphan.Count != 0 || planned.LegacyStaleAfterSeconds != defaultLegacyStaleAfter.Seconds() {
		t.Fatalf("legacy path defaulted on: %+v", planned.LegacyOrphan)
	}
	if !planned.JournalHostMatch || !strings.HasPrefix(planned.PlanDigest, "sha256:") || strings.Contains(stdout.String(), `"host_local"`) {
		t.Fatalf("plan digest=%s host field in %s", planned.PlanDigest, stdout.String())
	}
	if !strings.Contains(stdout.String(), "--plan-digest") || !strings.Contains(stdout.String(), planned.PlanDigest) || !strings.Contains(stdout.String(), "Multica issue state is not changed; local collection stamps are written") {
		t.Fatalf("plan next action: %s", stdout.String())
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
	applied := world.apply(t, app, stdout)
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
	orphanedRevision := orphaned.Revision
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := app.run(context.Background(), []string{"--output", "json", "--journal", world.path, "reconcile", "--apply", "--plan-digest", applied.PlanDigest}); code == 0 {
		t.Fatalf("reviewed digest still applied after the journal changed: %s", stdout.String())
	}
	journal = openReconcileJournal(t, world.path)
	deferred, err := journal.GetExecution(context.Background(), world.gone.ID)
	if err != nil || deferred.Revision != orphanedRevision {
		t.Fatalf("stale digest wrote: rev %d->%d err=%v", orphanedRevision, deferred.Revision, err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	again := world.apply(t, app, stdout)
	if again.Orphan.Count != 0 || again.Collect.Count != 0 || strings.Contains(stdout.String(), reconcileSecret) {
		t.Fatalf("replay was not empty: %s", stdout.String())
	}
}

func TestReconcileReportsMulticaBindingWithoutMutatingIt(t *testing.T) {
	world := seedReconcileWorld(t)
	stdout, app := reconcileApp(t, world)
	planned := world.apply(t, app, stdout)
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
	report := classifyReconcile([]model.Execution{execution}, store.AcknowledgementIndex{}, ids.HostID("this-host"), time.Now(), reconcileOptions{staleAfter: 24 * time.Hour, labels: []string{}}, func(processIdentity) processProof {
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
	if _, problem := parseReconcile([]string{"--apply"}); problem == nil {
		t.Fatal("apply without plan digest accepted")
	}
	if _, problem := parseReconcile([]string{"--plan", "--plan-digest", "sha256:" + strings.Repeat("ab", 32)}); problem == nil {
		t.Fatal("plan digest accepted on plan")
	}
	for _, value := range []string{"59s", "721h", "31d", "forever"} {
		if _, problem := parseReconcile([]string{"--plan", "--stale-after", value}); problem == nil {
			t.Fatalf("stale-after %q accepted", value)
		}
	}
	opts, problem := parseReconcile([]string{"--plan", "--stale-after", "24h", "--collect-older-than", "7d", "--include-failures", "--adapter", "cursor", "--label", "review"})
	if problem != nil || opts.staleAfter != 24*time.Hour || opts.collectOlder != 7*24*time.Hour || !opts.includeFailures || opts.includeLegacy || opts.legacyStaleAfter != defaultLegacyStaleAfter || opts.adapter != "cursor" || len(opts.labels) != 1 {
		t.Fatalf("options=%+v problem=%v", opts, problem)
	}
	for _, value := range []string{"71h", "71h59m", "2d", "8761h"} {
		if _, problem := parseReconcile([]string{"--plan", "--legacy-stale-after", value}); problem == nil {
			t.Fatalf("legacy-stale-after %q accepted", value)
		}
	}
	legacyOpts, problem := parseReconcile([]string{"--plan", "--include-legacy-unproven", "--legacy-stale-after", "72h"})
	if problem != nil || !legacyOpts.includeLegacy || legacyOpts.legacyStaleAfter != 72*time.Hour || legacyOpts.legacyStaleAfterRaw != "72h" {
		t.Fatalf("legacy options=%+v problem=%v", legacyOpts, problem)
	}
	days, problem := parseReconcile([]string{"--plan", "--include-legacy-unproven", "--legacy-stale-after", "3d"})
	if problem != nil || days.legacyStaleAfter != minimumLegacyStaleAfter {
		t.Fatalf("3d legacy=%s problem=%v", days.legacyStaleAfter, problem)
	}
	week, err := parseReconcileDuration("7d")
	if err != nil || week != 168*time.Hour {
		t.Fatalf("7d=%s err=%v", week, err)
	}
}

func TestReconcileDigestMismatchWritesNothing(t *testing.T) {
	world := seedReconcileWorld(t)
	stdout, app := reconcileApp(t, world)
	planned := world.run(t, app, stdout)
	stdout.Reset()
	if code := app.run(context.Background(), []string{"--output", "json", "--journal", world.path, "reconcile", "--apply", "--plan-digest", planned.PlanDigest + "00"}); code == 0 {
		t.Fatalf("mismatched digest applied: %s", stdout.String())
	}
	assertUntouched(t, world.path, world.gone, world.alive, world.plain, world.multica)
}

func TestReconcileAliveAtApplyLeavesRowUnchanged(t *testing.T) {
	world := seedReconcileWorld(t)
	stdout, app := reconcileApp(t, world)
	calls := map[int]int{}
	app.processProof = func(id processIdentity) processProof {
		calls[id.PID]++
		if id.PID == 4242 && calls[id.PID] >= 3 {
			return processProof{Proof: "pid_alive", Alive: true, Present: true}
		}
		switch id.PID {
		case 4242, 4646:
			return processProof{Proof: "pid_absent", Gone: true}
		case 4343:
			return processProof{Proof: "pid_alive", Alive: true, Present: true}
		case 4848:
			return processProof{Proof: "start_unproven", Present: true}
		default:
			t.Errorf("unexpected process proof for pid %d", id.PID)
			return processProof{Proof: "start_unproven"}
		}
	}
	applied := world.apply(t, app, stdout)
	if sameIDs(idsOfOrphans(applied), world.gone.ID) || !sameIDs(idsOfOrphans(applied), world.codex.ID) {
		t.Fatalf("orphans=%v", idsOfOrphans(applied))
	}
	journal := openReconcileJournal(t, world.path)
	defer journal.Close()
	kept, err := journal.GetExecution(context.Background(), world.gone.ID)
	if err != nil || kept.State != model.StateRunning || kept.Revision != world.gone.Revision {
		t.Fatalf("pid that returned was changed: %+v err=%v", kept, err)
	}
}

func TestReconcileRevisionConflictDuringApplyIsSkipped(t *testing.T) {
	world := seedReconcileWorld(t)
	stdout, app := reconcileApp(t, world)
	if planned := world.run(t, app, stdout); planned.Orphan.Count == 0 {
		t.Fatal("plan had no orphans")
	}
	journal := openReconcileJournal(t, world.path)
	defer journal.Close()
	host, err := journal.HostID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	executions, err := journal.ListExecutions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	acks, err := journal.AcknowledgementIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	opts := reconcileOptions{
		apply: true, staleAfter: defaultReconcileStaleAfter, collectOlder: defaultReconcileCollectAfter,
		legacyStaleAfter: defaultLegacyStaleAfter, labels: []string{},
	}
	report := classifyReconcile(executions, acks, host, world.now, opts, app.proveProcess)
	before, err := journal.GetExecution(context.Background(), world.gone.ID)
	if err != nil {
		t.Fatal(err)
	}
	app.processProof = func(id processIdentity) processProof {
		if id.PID == 4242 {
			current, readErr := journal.GetExecution(context.Background(), world.gone.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			current.UpdatedAt = current.UpdatedAt.Add(time.Second)
			if _, updateErr := journal.UpdateExecution(context.Background(), current, current.Revision); updateErr != nil {
				t.Fatal(updateErr)
			}
		}
		switch id.PID {
		case 4242, 4646:
			return processProof{Proof: "pid_absent", Gone: true}
		case 4343:
			return processProof{Proof: "pid_alive", Alive: true, Present: true}
		case 4848:
			return processProof{Proof: "start_unproven", Present: true}
		default:
			t.Errorf("unexpected process proof for pid %d", id.PID)
			return processProof{Proof: "start_unproven"}
		}
	}
	if problem := app.applyReconcile(context.Background(), journal, host, world.now, opts, &report); problem != nil {
		t.Fatal(problem)
	}
	if sameIDs(idsOfOrphans(report), world.gone.ID) || !sameIDs(idsOfOrphans(report), world.codex.ID) {
		t.Fatalf("conflicted row was not skipped: %v", idsOfOrphans(report))
	}
	kept, err := journal.GetExecution(context.Background(), world.gone.ID)
	if err != nil || kept.State != model.StateRunning || kept.Revision != before.Revision+1 {
		t.Fatalf("conflicted row changed: rev %d->%d state %s err=%v", before.Revision, kept.Revision, kept.State, err)
	}
	orphaned, err := journal.GetExecution(context.Background(), world.codex.ID)
	if err != nil || orphaned.State != model.StateOrphaned {
		t.Fatalf("sibling was not orphaned: %+v err=%v", orphaned, err)
	}
}

func TestNumericSessionBindingIsUnproven(t *testing.T) {
	observed := time.Now().Add(-48 * time.Hour)
	session := "8675309"
	execution := model.Execution{
		OriginHostID: "this-host", Authority: model.AuthorityNative, State: model.StateRunning,
		SourceBindings: []model.SourceBinding{{Kind: "cursor_session", OpaqueID: &session}},
		Observation:    model.Observation{ObservedAt: observed},
	}
	calls := 0
	report := classifyReconcile([]model.Execution{execution}, store.AcknowledgementIndex{}, "this-host", time.Now(), reconcileOptions{staleAfter: 24 * time.Hour, labels: []string{}}, func(processIdentity) processProof {
		calls++
		return processProof{Proof: "pid_absent", Gone: true}
	})
	if calls != 0 || report.Orphan.Count != 0 || len(report.Unproven) != 1 || report.Unproven[0].Reason != "ownership_unproven" {
		t.Fatalf("numeric session id was treated as a pid: calls=%d %+v", calls, report)
	}
	started := observed
	execution.Launch = &model.LaunchIdentity{PID: 4242, StartedAt: started}
	proved := 0
	report = classifyReconcile([]model.Execution{execution}, store.AcknowledgementIndex{}, "this-host", time.Now(), reconcileOptions{staleAfter: 24 * time.Hour, labels: []string{}}, func(id processIdentity) processProof {
		proved = id.PID
		return processProof{Proof: "pid_absent", Gone: true}
	})
	if proved != 4242 || report.Orphan.Count != 1 {
		t.Fatalf("launcher pid was not used: proved=%d %+v", proved, report)
	}
}

func TestBulkAcknowledgementSourceIsVisible(t *testing.T) {
	world := seedReconcileWorld(t)
	stdout, app := reconcileApp(t, world)
	if applied := world.apply(t, app, stdout); applied.Collect.Count == 0 {
		t.Fatalf("nothing collected: %+v", applied.Collect)
	}
	journal := openReconcileJournal(t, world.path)
	if _, _, err := journal.AcknowledgeExecution(context.Background(), world.recentTerminal.ID, store.AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ListEvents(context.Background(), world.plain.ID, contracts.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	foundEvent := false
	for _, event := range events {
		if event.Kind == model.EventAcknowledged && event.Payload["acknowledgement_source"] == store.AcknowledgementBulk {
			foundEvent = true
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if !foundEvent {
		t.Fatalf("bulk stamp did not emit an event: %+v", events)
	}
	stdout.Reset()
	if code := app.run(context.Background(), []string{"--output", "json", "--journal", world.path, "recent"}); code != 0 {
		t.Fatalf("recent exit=%d %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"acknowledgement_source":"bulk_reconciled"`) || !strings.Contains(stdout.String(), `"acknowledgement_source":"result"`) {
		t.Fatalf("recent hid acknowledgement source: %s", stdout.String())
	}
	stdout.Reset()
	if code := app.run(context.Background(), []string{"--output", "json", "--journal", world.path, "status", world.plain.ID.String()}); code != 0 {
		t.Fatalf("status exit=%d %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"acknowledgement_source":"bulk_reconciled"`) {
		t.Fatalf("status hid bulk acknowledgement: %s", stdout.String())
	}
}

func TestReconcileLegacyUnprovenRules(t *testing.T) {
	now := time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)
	hostRaw, err := ids.New(ids.TypeHost)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ids.ParseHostID(hostRaw.String())
	if err != nil {
		t.Fatal(err)
	}
	foreignRaw, err := ids.New(ids.TypeHost)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := ids.ParseHostID(foreignRaw.String())
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-defaultLegacyStaleAfter)
	younger := now.Add(-defaultLegacyStaleAfter + time.Hour)
	between := now.Add(-48 * time.Hour)
	base := reconcileOptions{staleAfter: defaultReconcileStaleAfter, collectOlder: defaultReconcileCollectAfter, legacyStaleAfter: defaultLegacyStaleAfter, labels: []string{}}

	t.Run("unreadable legacy pid is never treated as absent", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		report := classifyReconcile([]model.Execution{legacyFixture(t, host, "cursor_session", "4242", old, false)}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			return processProof{Proof: "permission_denied"}
		})
		if report.LegacyOrphan.Count != 0 || len(report.Unproven) != 1 || report.Unproven[0].Reason != "legacy_pid_unproven" {
			t.Fatalf("report=%+v", report)
		}
	})
	t.Run("flag off leaves the row unproven", func(t *testing.T) {
		calls := 0
		report := classifyReconcile([]model.Execution{legacyFixture(t, host, "cursor_session", "4242", old, false)}, store.AcknowledgementIndex{}, host, now, base, func(processIdentity) processProof {
			calls++
			return processProof{Present: true}
		})
		if calls != 0 || report.LegacyOrphan.Count != 0 || len(report.Unproven) != 1 || report.Unproven[0].Reason != "ownership_unproven" {
			t.Fatalf("calls=%d report=%+v", calls, report)
		}
	})

	t.Run("observation younger than legacy-stale-after stays unproven", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		calls := 0
		report := classifyReconcile([]model.Execution{legacyFixture(t, host, "cursor_session", "4242", younger, false)}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			calls++
			return processProof{}
		})
		if calls != 0 || report.LegacyOrphan.Count != 0 || report.Unproven[0].Reason != "ownership_unproven" {
			t.Fatalf("calls=%d %+v", calls, report)
		}
	})

	t.Run("age between stale-after and legacy-stale-after stays unproven", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		report := classifyReconcile([]model.Execution{legacyFixture(t, host, "process", "not-a-pid", between, false)}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			t.Fatal("proved a row that is not legacy-eligible")
			return processProof{}
		})
		if report.LegacyOrphan.Count != 0 || report.Unchanged.Recent != 0 || report.Unproven[0].Reason != "ownership_unproven" {
			t.Fatalf("%+v", report)
		}
	})

	t.Run("no numeric value records heartbeat_absent", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		row := legacyFixture(t, host, "cursor_session", "chat-1", old, false)
		report := classifyReconcile([]model.Execution{row}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			t.Fatal("proved a row with no numeric value")
			return processProof{}
		})
		if report.LegacyOrphan.Count != 1 || report.Orphan.Count != 0 {
			t.Fatalf("%+v", report)
		}
		item := report.LegacyOrphan.Executions[0]
		if item.ID != row.ID || item.Reason != "owner_unproven_legacy" || item.Evidence.Proof != "heartbeat_absent" || item.Evidence.PID != 0 {
			t.Fatalf("%+v", item)
		}
	})

	t.Run("absent numeric pid is heartbeat evidence not pid proof", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		var saw processIdentity
		report := classifyReconcile([]model.Execution{legacyFixture(t, host, "codex_thread", "4242", old, false)}, store.AcknowledgementIndex{}, host, now, opts, func(id processIdentity) processProof {
			saw = id
			return processProof{Proof: "pid_absent", Gone: true}
		})
		item := report.LegacyOrphan.Executions[0]
		if saw.PID != 4242 || !saw.StartedAt.IsZero() || item.Evidence.Proof != "heartbeat_absent" || item.Evidence.PID != 4242 || item.Reason != "owner_unproven_legacy" {
			t.Fatalf("saw=%+v item=%+v", saw, item)
		}
	})

	t.Run("live pid blocks regardless of start time", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		for _, proof := range []processProof{
			{Present: true, Proof: "pid_reused", Gone: true},
			{Present: true, Proof: "start_unproven"},
		} {
			var saw processIdentity
			report := classifyReconcile([]model.Execution{legacyFixture(t, host, "cursor_session", "4242", old, false)}, store.AcknowledgementIndex{}, host, now, opts, func(id processIdentity) processProof {
				saw = id
				return proof
			})
			if !saw.StartedAt.IsZero() || report.LegacyOrphan.Count != 0 || report.Orphan.Count != 0 || len(report.Unproven) != 1 || report.Unproven[0].Reason != "legacy_pid_present" {
				t.Fatalf("proof=%+v saw=%+v report=%+v", proof, saw, report)
			}
		}
	})

	t.Run("a later live pid blocks", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		row := legacyFixture(t, host, "process", "1111", old, false)
		second := "2222"
		row.SourceBindings = append(row.SourceBindings, testSourceBinding(t, "process", ids.TypeSource, second))
		seen := []int{}
		report := classifyReconcile([]model.Execution{row}, store.AcknowledgementIndex{}, host, now, opts, func(id processIdentity) processProof {
			seen = append(seen, id.PID)
			if !id.StartedAt.IsZero() {
				t.Fatal("compared start time")
			}
			if id.PID == 2222 {
				return processProof{Present: true, Proof: "pid_reused", Gone: true}
			}
			return processProof{Proof: "pid_absent", Gone: true}
		})
		if len(seen) != 2 || report.LegacyOrphan.Count != 0 || report.Unproven[0].Reason != "legacy_pid_present" {
			t.Fatalf("seen=%v %+v", seen, report)
		}
	})

	t.Run("noncanonical numeric value is not a pid", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		report := classifyReconcile([]model.Execution{legacyFixture(t, host, "process", "04242", old, false)}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			t.Fatal("treated a noncanonical value as a pid")
			return processProof{Present: true}
		})
		if report.LegacyOrphan.Count != 1 || report.LegacyOrphan.Executions[0].Evidence.PID != 0 || report.LegacyOrphan.Executions[0].Evidence.Proof != "heartbeat_absent" {
			t.Fatalf("%+v", report.LegacyOrphan)
		}
	})

	t.Run("numeric value outside a launch kind does not block", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		report := classifyReconcile([]model.Execution{legacyFixture(t, host, "note", "4242", old, false)}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			t.Fatal("proved a non-launch binding")
			return processProof{Present: true}
		})
		if report.LegacyOrphan.Count != 1 || report.Unproven != nil && len(report.Unproven) != 0 {
			t.Fatalf("%+v", report)
		}
	})

	t.Run("foreign host is not a legacy orphan", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		report := classifyReconcile([]model.Execution{legacyFixture(t, foreign, "process", "4242", old, false)}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			t.Fatal("proved a foreign host")
			return processProof{Gone: true}
		})
		if report.LegacyOrphan.Count != 0 || report.Unproven[0].Reason != "foreign_host" {
			t.Fatalf("%+v", report)
		}
	})

	t.Run("launch record stays on the owner_lost path", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		var saw processIdentity
		report := classifyReconcile([]model.Execution{legacyFixture(t, host, "cursor_session", "4242", old, true)}, store.AcknowledgementIndex{}, host, now, opts, func(id processIdentity) processProof {
			saw = id
			return processProof{Proof: "pid_absent", Gone: true}
		})
		if report.LegacyOrphan.Count != 0 || report.Orphan.Count != 1 || report.Orphan.Executions[0].Reason != "owner_lost" || saw.PID != 4242 || saw.StartedAt.IsZero() {
			t.Fatalf("saw=%+v %+v", saw, report)
		}
	})

	t.Run("multica is not a legacy orphan", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		row := legacyFixture(t, host, "multica_issue", "4242", old, false)
		row.Authority = model.AuthorityMultica
		row.Adapter = "multica"
		report := classifyReconcile([]model.Execution{row}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			t.Fatal("proved multica")
			return processProof{}
		})
		if report.LegacyOrphan.Count != 0 || report.Orphan.Count != 0 {
			t.Fatalf("%+v", report)
		}
	})

	t.Run("active runner lease is unchanged", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		row := legacyFixture(t, host, "process", "4242", old, false)
		lease := 400 * 3600
		row.Liveness = model.LivenessAlive
		row.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: old, FreshForSeconds: &lease}
		report := classifyReconcile([]model.Execution{row}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			t.Fatal("proved a live lease")
			return processProof{Gone: true}
		})
		if report.LegacyOrphan.Count != 0 || report.Unchanged.Recent != 1 || len(report.Unproven) != 0 {
			t.Fatalf("%+v", report)
		}
	})

	t.Run("minimum legacy age is accepted at the boundary", func(t *testing.T) {
		opts := base
		opts.includeLegacy = true
		opts.legacyStaleAfter = minimumLegacyStaleAfter
		row := legacyFixture(t, host, "claude_session", "session", now.Add(-minimumLegacyStaleAfter), false)
		report := classifyReconcile([]model.Execution{row}, store.AcknowledgementIndex{}, host, now, opts, func(processIdentity) processProof {
			t.Fatal("proved a non-numeric row")
			return processProof{}
		})
		if report.LegacyOrphan.Count != 1 || report.LegacyOrphan.Executions[0].Evidence.Proof != "heartbeat_absent" {
			t.Fatalf("%+v", report.LegacyOrphan)
		}
	})
}

func TestReconcileLegacyApplyRecordsUnprovenOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Minute)
	observed := now.Add(-200 * time.Hour)
	row := mustCreateLegacyRow(t, journal, "cursor_session", "5151", observed)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := testApp(&stdout, &stderr)
	app.now = func() time.Time { return now }
	wrapped := &reconcileTestApp{app: app, processCalls: map[int]int{}}
	app.processProof = func(id processIdentity) processProof {
		wrapped.processCalls[id.PID]++
		if !id.StartedAt.IsZero() {
			t.Errorf("legacy proof compared start time %s", id.StartedAt)
		}
		return processProof{Proof: "pid_absent", Gone: true}
	}
	planned := reconcileWorld{path: path, now: now}.run(t, wrapped, &stdout, "--include-legacy-unproven")
	if planned.LegacyOrphan.Count != 1 || planned.Orphan.Count != 0 || !strings.Contains(planned.PlanDigest, "sha256:") {
		t.Fatalf("plan=%+v", planned.LegacyOrphan)
	}
	item := planned.LegacyOrphan.Executions[0]
	if item.ID != row.ID || item.Reason != "owner_unproven_legacy" || item.Evidence.Proof != "heartbeat_absent" || item.Evidence.PID != 5151 {
		t.Fatalf("item=%+v", item)
	}
	if !strings.Contains(stdout.String(), "--include-legacy-unproven") || !strings.Contains(stdout.String(), planned.PlanDigest) {
		t.Fatalf("next action missing legacy flag: %s", stdout.String())
	}
	applied := reconcileWorld{path: path, now: now}.apply(t, wrapped, &stdout, "--include-legacy-unproven")
	if applied.LegacyOrphan.Count != 1 || strings.Contains(stdout.String(), `"owner_lost"`) {
		t.Fatalf("apply=%s", stdout.String())
	}
	journal = openReconcileJournal(t, path)
	stored, err := journal.GetExecution(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != model.StateOrphaned || stored.SourceState == nil || *stored.SourceState != "owner_unproven_legacy" {
		t.Fatalf("stored=%+v", stored)
	}
	outcome, err := journal.GetOutcome(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Content != nil || outcome.Failure == nil || outcome.Failure.Code != "owner_unproven_legacy" || outcome.Failure.Kind != "observation" || outcome.NativeExitCode != nil || strings.Contains(outcome.Failure.Code, "owner_lost") {
		t.Fatalf("outcome=%+v", outcome)
	}
	events, err := journal.ListEvents(context.Background(), row.ID, contracts.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == model.EventTerminal && event.Payload["proof"] == "heartbeat_absent" && event.Payload["diagnostic_code"] == "owner_unproven_legacy" {
			found = true
			if event.Payload["reason"] == "owner_lost" || event.Payload["diagnostic_code"] == "owner_lost" {
				t.Fatalf("legacy event used owner_lost: %+v", event.Payload)
			}
		}
	}
	if !found {
		t.Fatalf("events=%+v", events)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	again := reconcileWorld{path: path, now: now}.apply(t, wrapped, &stdout, "--include-legacy-unproven")
	if again.LegacyOrphan.Count != 0 || again.Orphan.Count != 0 {
		t.Fatalf("replay wrote again: %+v", again.LegacyOrphan)
	}
}

func TestReconcileLegacyPIDPresentIsLeftUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Minute)
	row := mustCreateLegacyRow(t, journal, "process", "6161", now.Add(-200*time.Hour))
	revision := row.Revision
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := testApp(&stdout, &stderr)
	app.now = func() time.Time { return now }
	wrapped := &reconcileTestApp{app: app, processCalls: map[int]int{}}
	app.processProof = func(id processIdentity) processProof {
		wrapped.processCalls[id.PID]++
		if !id.StartedAt.IsZero() {
			t.Errorf("compared start time")
		}
		return processProof{Present: true, Proof: "pid_reused", Gone: true}
	}
	planned := reconcileWorld{path: path, now: now}.run(t, wrapped, &stdout, "--include-legacy-unproven")
	if planned.LegacyOrphan.Count != 0 || len(planned.Unproven) != 1 || planned.Unproven[0].Reason != "legacy_pid_present" || planned.Unproven[0].ID != row.ID {
		t.Fatalf("plan=%s", stdout.String())
	}
	applied := reconcileWorld{path: path, now: now}.apply(t, wrapped, &stdout, "--include-legacy-unproven")
	if applied.LegacyOrphan.Count != 0 {
		t.Fatalf("present pid was orphaned: %s", stdout.String())
	}
	journal = openReconcileJournal(t, path)
	defer journal.Close()
	kept, err := journal.GetExecution(context.Background(), row.ID)
	if err != nil || kept.State != model.StateRunning || kept.Revision != revision {
		t.Fatalf("kept=%+v err=%v", kept, err)
	}
}

func TestReconcileLegacyPIDAppearingAtApplyIsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Minute)
	row := mustCreateLegacyRow(t, journal, "omp_session", "7171", now.Add(-200*time.Hour))
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := testApp(&stdout, &stderr)
	app.now = func() time.Time { return now }
	calls := 0
	app.processProof = func(id processIdentity) processProof {
		calls++
		if !id.StartedAt.IsZero() {
			t.Errorf("compared start time")
		}
		if calls >= 3 {
			return processProof{Present: true, Proof: "pid_reused", Gone: true}
		}
		return processProof{Proof: "pid_absent", Gone: true}
	}
	wrapped := &reconcileTestApp{app: app, processCalls: map[int]int{}}
	applied := reconcileWorld{path: path, now: now}.apply(t, wrapped, &stdout, "--include-legacy-unproven")
	if applied.LegacyOrphan.Count != 0 {
		t.Fatalf("pid that appeared was written: %s", stdout.String())
	}
	journal = openReconcileJournal(t, path)
	defer journal.Close()
	kept, err := journal.GetExecution(context.Background(), row.ID)
	if err != nil || kept.State != model.StateRunning || kept.Revision != row.Revision {
		t.Fatalf("kept=%+v err=%v", kept, err)
	}
}

func legacyFixture(t *testing.T, host ids.HostID, kind, opaque string, observed time.Time, launch bool) model.Execution {
	t.Helper()
	raw, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ids.ParseExecutionID(raw.String())
	if err != nil {
		t.Fatal(err)
	}
	execution := model.Execution{
		ID: id, OriginHostID: host, Authority: model.AuthorityNative, Adapter: "cursor",
		State: model.StateRunning, Liveness: model.LivenessUnreachable,
		SourceBindings: []model.SourceBinding{testSourceBinding(t, kind, ids.TypeSource, opaque)},
		Observation:    model.Observation{ObservedAt: observed},
	}
	if launch {
		pid, convErr := strconv.Atoi(opaque)
		if convErr != nil {
			t.Fatal(convErr)
		}
		execution.Launch = &model.LaunchIdentity{PID: pid, StartedAt: observed}
	}
	return execution
}

func mustCreateLegacyRow(t *testing.T, journal *store.Journal, kind, opaque string, observed time.Time) model.Execution {
	t.Helper()
	created := mustCreateRunning(t, journal, "cursor", kind, opaque, observed, nil)
	if created.Launch != nil {
		created.Launch = nil
		var err error
		created, err = journal.UpdateExecution(context.Background(), created, created.Revision)
		if err != nil {
			t.Fatal(err)
		}
	}
	return created
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
	base.processProof = func(id processIdentity) processProof {
		wrapped.processCalls[id.PID]++
		switch id.PID {
		case 4242, 4646:
			return processProof{Proof: "pid_absent", Gone: true}
		case 4343:
			return processProof{Proof: "pid_alive", Alive: true, Present: true}
		case 4848:
			return processProof{Proof: "start_unproven", Present: true}
		default:
			t.Errorf("unexpected process proof for pid %d", id.PID)
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

func (w reconcileWorld) apply(t *testing.T, app *reconcileTestApp, stdout *bytes.Buffer, extra ...string) reconcileReport {
	t.Helper()
	planned := w.run(t, app, stdout, append([]string{"--plan"}, extra...)...)
	args := append(append([]string{}, extra...), "--apply", "--plan-digest", planned.PlanDigest)
	return w.run(t, app, stdout, args...)
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
	if pid, err := strconv.Atoi(opaque); err == nil && strconv.Itoa(pid) == opaque && pid > 0 && pid <= model.MaxLaunchPID {
		execution.Launch = &model.LaunchIdentity{PID: pid, StartedAt: started}
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
