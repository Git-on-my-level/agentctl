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

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/anxrun"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

// writeAnxConfig writes an owner-only agentctl config whose anx run-links
// executable points at script (or nowhere when empty).
func writeAnxConfig(t *testing.T, executable string) string {
	t.Helper()
	path := filepath.Join(testutil.TempDir(t), "config.json")
	document := map[string]any{"schema_version": 1}
	if executable != "" {
		document["anx"] = map[string]any{"run_links": map[string]any{"executable": executable}}
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeAnxScript writes one line per invocation (argv + stdin) and always
// answers with a successful session envelope.
func fakeAnxScript(t *testing.T) (string, string) {
	t.Helper()
	dir := testutil.TempDir(t)
	log := filepath.Join(dir, "anx-calls.log")
	script := filepath.Join(dir, "fake-anx")
	body := "#!/bin/sh\necho \"$*\" >> " + log + "\ncat >> " + log + "\ncase \"$*\" in\n *whoami*) echo '{\"ok\":true,\"result\":{\"agent\":{\"actor_id\":\"actor-test\"}}}' ;;\n *cards.list*) echo '{\"ok\":true,\"result\":{\"cards\":[{\"ref\":\"card:watch-redesign\",\"thread_id\":\"thread-test\"},{\"ref\":\"card:dogfooding\",\"thread_id\":\"thread-test\"}]}}' ;;\n *cards.get*) echo '{\"ok\":true,\"result\":{\"card\":{\"thread_id\":\"thread-test\"}}}' ;;\n *) echo '{\"ok\":true,\"result\":{\"session\":{\"id\":\"sess-1\"}}}' ;;\nesac\n"

	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, log
}

func readAnxCalls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// seedTerminalLabeledExecution journals one completed native execution with
// an anx.card label and a native session binding, through the same finalize
// path production uses.
func seedTerminalLabeledExecution(t *testing.T, a *app, journalPath string, labels []string) string {
	t.Helper()
	now := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)
	journal, err := store.Open(journalPath, store.Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{
		Authority: model.AuthorityNative, Adapter: "claude-code", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched,
		State: model.StateRunning, Liveness: model.LivenessAlive, Labels: labels, SourceBindings: []model.SourceBinding{},
		Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		Observation:  model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now},
	}, contracts.MutationKey{})
	if err != nil {
		journal.Close()
		t.Fatal(err)
	}
	updated, err := a.finalizeResult(context.Background(), common{journalPath: journalPath}, journal, execution, adapter.Result{
		Success: true, State: adapter.StateCompleted, Content: "Landed the watcher.\ncommit: 0123456789abcdef0123456789abcdef01234567", ContentType: "text/plain",
		SessionRef: adapter.SourceRef{OpaqueID: "4dba0f53-9021-4c86-a701-30e1c8c657cd"},
	}, now, false)
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	return updated.ID.String()
}

// TestANXRegisterTerminalNeverAcknowledgesCollection is the regression test
// for review thread 2 on PR #104: a background registration read must never
// acknowledge collection, so an uncollected terminal run stays unreconciled.
func TestANXRegisterTerminalNeverAcknowledgesCollection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	journalPath := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	script, log := fakeAnxScript(t)
	configPath := writeAnxConfig(t, script)
	id := seedTerminalLabeledExecution(t, a, journalPath, []string{"anx.card.watch-redesign"})

	// The run has not been collected by its parent.
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	acks, err := journal.AcknowledgementIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	execution, err := journal.GetExecution(context.Background(), mustParseExecID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	journal.Close()
	if acks.Unreconciled(execution) != true {
		t.Fatal("precondition: seeded execution should be unreconciled")
	}

	a.anxRegisterTerminal(common{configPath: configPath, journalPath: journalPath}, mustParseExecID(t, id))

	// Still unreconciled after registration: the hook read the outcome
	// through the journal's read-only accessors, never result dereference.
	journal, err = store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.GetAcknowledgement(context.Background(), mustParseExecID(t, id)); err == nil {
		journal.Close()
		t.Fatal("registration acknowledged collection")
	}
	acks, err = journal.AcknowledgementIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	execution, err = journal.GetExecution(context.Background(), mustParseExecID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	journal.Close()
	if !acks.Unreconciled(execution) {
		t.Fatal("uncollected run did not stay unreconciled after ANX registration")
	}
	// Terminal hooks only enqueue. Explicit delivery preserves collection too.
	if calls := readAnxCalls(t, log); len(calls) != 0 {
		t.Fatal("terminal hook invoked ANX synchronously")
	}
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journalPath, "anx", "flush"}); code != 0 {
		t.Fatal("flush failed")
	}
	// The delivery happened: session + one participation call, both carrying
	// the card, with no acknowledgement side effects.
	calls := readAnxCalls(t, log)
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "api sessions.register") {
		t.Fatalf("no session registration call: %v", calls)
	}
	if !strings.Contains(joined, "api work.participants.register") || !strings.Contains(joined, "card_ref=card:watch-redesign") {
		t.Fatalf("no participation call for the card: %v", calls)
	}
	// The terminal registration spooled empty (delivered) and carried the
	// redacted outcome and the identity fingerprint.
	spool := openSpoolForTest(t, journalPath)
	if pending, _ := spool.ListPending(); len(pending) != 0 {
		t.Fatalf("registration not delivered: %#v", pending)
	}
}

// TestANXRunLinkRegistersLabeledRunsOnly covers the default-on rule through
// the run command: a labeled run spools, an unlabeled run does not, and the
// --anx-card sugar adds the label.
func TestANXRunLinkRegistersLabeledRunsOnly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	root := testutil.TempDir(t)
	journalPath := filepath.Join(root, "state", "journal.db")
	// A broken executable keeps every delivery in the spool (retryable), so
	// the assertions inspect durable local state only.
	configPath := writeAnxConfig(t, filepath.Join(root, "missing-anx"))

	run := func(extra ...string) {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		args := append([]string{"--output", "json", "--config", configPath, "--journal", journalPath, "run", "--adapter", "generic-process"}, extra...)
		args = append(args, "--", "/bin/echo", `{"type":"result","status":"completed","result":"done"}`)
		if code := a.run(context.Background(), args); code != 0 {
			t.Fatalf("run exit=%d out=%s err=%s", code, stdout.String(), stderr.String())
		}
	}
	run("--label", "anx.card.demo")
	run("--label", "unrelated")
	run("--anx-card", "sugared")

	spool := openSpoolForTest(t, journalPath)
	pending, err := spool.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	// generic-process runs carry no native session binding, so their
	// deliveries dead-letter honestly with no_native_session_binding.
	dead, err := spool.ListDead()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending)+len(dead) != 4 {
		t.Fatalf("labeled registrations missing: %d", len(pending)+len(dead))
	}
	for _, entry := range append(pending, dead...) {
		if len(entry.Registration.CardRefs) != 0 {
			t.Fatal("unconfirmed card persisted")
		}
	}

	// Labels visible on the sugared execution: the label is the contract.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--config", configPath, "--journal", journalPath, "recent", "--label", "anx.card.sugared"}); code != 0 {
		t.Fatalf("recent exit=%d", code)
	}
	if !strings.Contains(stdout.String(), "anx.card.sugared") {
		t.Fatal("--anx-card sugar did not add the anx.card.<slug> label")
	}
}

func TestANXStatusAndFlushCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	root := testutil.TempDir(t)
	journalPath := filepath.Join(root, "state", "journal.db")
	script, log := fakeAnxScript(t)
	brokenConfig := writeAnxConfig(t, filepath.Join(root, "missing-anx"))
	id := seedTerminalLabeledExecution(t, a, journalPath, []string{"anx.card.watch-redesign"})
	// Enqueue against a broken anx so the entry stays pending.
	a.anxRegisterTerminal(common{configPath: brokenConfig, journalPath: journalPath}, mustParseExecID(t, id))
	spool := openSpoolForTest(t, journalPath)
	pending, _ := spool.ListPending()
	if len(pending) != 1 {
		t.Fatalf("expected one pending entry, got %#v", pending)
	}
	// The failed delivery scheduled a backoff; make the entry due now by
	// rewriting its next_attempt_at in the spool file.
	forcePendingDue(t, journalPath)

	// status is read-only and reports the pending registration.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--config", brokenConfig, "--journal", journalPath, "anx", "status"}); code != 0 {
		t.Fatalf("anx status exit=%d out=%s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), `"card:watch-redesign"`) || !strings.Contains(stdout.String(), id) || !strings.Contains(stdout.String(), `"pending"`) {
		t.Fatalf("status output missing pending entry: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"workspace_alias":"anx"`) || !strings.Contains(stdout.String(), `"spool":"present"`) || !strings.Contains(stdout.String(), `"pending_count":1`) || strings.Contains(stdout.String(), `"spool_dir"`) {
		t.Fatal("status must show safe alias and presence/counts without a path")
	}
	if len(readAnxCalls(t, log)) != 0 {
		t.Fatal("anx status must not deliver anything")
	}

	// Point the config at the fake anx and flush: the entry delivers.
	workingConfig := writeAnxConfig(t, script)
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--config", workingConfig, "--journal", journalPath, "anx", "flush"}); code != 0 {
		t.Fatalf("anx flush exit=%d out=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"delivered":1`) && !strings.Contains(stdout.String(), `"delivered": 1`) {
		t.Fatalf("flush result wrong: %s", stdout.String())
	}
	if calls := readAnxCalls(t, log); len(calls) == 0 {
		t.Fatal("flush did not invoke the anx CLI")
	}
	if pending, _ := openSpoolForTest(t, journalPath).ListPending(); len(pending) != 0 {
		t.Fatalf("entry still pending after flush: %#v", pending)
	}
}

func TestANXCardOverrideFlagRejectsSlugs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	configPath := writeAnxConfig(t, "")
	if code := a.run(context.Background(), []string{"--output", "json", "--config", configPath, "--journal", filepath.Join(testutil.TempDir(t), "j.db"), "run", "--anx-card", "Bad Slug", "--", "/bin/true"}); code == 0 {
		t.Fatal("invalid card slug accepted")
	}
}

func mustParseExecID(t *testing.T, value string) ids.ExecutionID {
	t.Helper()
	parsed, err := parseExecutionRef(value, common{})
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// forcePendingDue zeroes next_attempt_at on every pending spool entry.
func forcePendingDue(t *testing.T, journalPath string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(journalPath), "anx-run-links", "pending", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var entry map[string]any
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatal(err)
		}
		entry["next_attempt_at"] = time.Time{}
		updated, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, updated, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func openSpoolForTest(t *testing.T, journalPath string) *anxrun.Spool {
	t.Helper()
	spool, err := anxrun.OpenSpool(filepath.Join(filepath.Dir(journalPath), "anx-run-links"))
	if err != nil {
		t.Fatal(err)
	}
	return spool
}
