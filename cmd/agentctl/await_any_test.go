package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

type awaitAnyDocument struct {
	OK     bool `json:"ok"`
	Result struct {
		ExecutionID    string         `json:"execution_id"`
		State          string         `json:"state"`
		MatchedKind    string         `json:"matched_kind"`
		TriggerAt      string         `json:"trigger_at"`
		PendingMatches int            `json:"pending_matches"`
		Watched        int            `json:"watched"`
		Acknowledged   bool           `json:"acknowledged"`
		Selector       map[string]any `json:"selector"`
	} `json:"result"`
	Error struct {
		Code     string         `json:"code"`
		ExitCode int            `json:"exit_code"`
		Details  map[string]any `json:"details"`
	} `json:"error"`
}

func runLabeled(t *testing.T, journalPath, label string, argv ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	args := []string{"--output", "json", "--journal", journalPath, "run", "--adapter", "generic-process", "--allow-missing-result"}
	if label != "" {
		args = append(args, "--label", label)
	}
	args = append(args, "--")
	args = append(args, argv...)
	code := a.run(context.Background(), args)
	var doc struct {
		Result model.Execution `json:"result"`
		Error  struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("run exit=%d output=%s: %v", code, stdout.String(), err)
	}
	if doc.Result.ID.String() != "" && doc.Result.ID.String() != "exec-" {
		return doc.Result.ID.String()
	}
	if id, ok := doc.Error.Details["execution_id"].(string); ok {
		return id
	}
	t.Fatalf("run exit=%d returned no execution: %s", code, stdout.String())
	return ""
}

func completedRun(t *testing.T, journalPath, label string) string {
	t.Helper()
	return runLabeled(t, journalPath, label, "/bin/echo", `{"type":"result","status":"completed","result":"fan-in answer"}`)
}

func awaitAny(t *testing.T, journalPath string, args ...string) (int, awaitAnyDocument, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), append([]string{"--output", "json", "--journal", journalPath, "await-any"}, args...))
	var doc awaitAnyDocument
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("await-any exit=%d output=%s: %v", code, stdout.String(), err)
	}
	return code, doc, stdout.String()
}

func unreconciledTotal(t *testing.T, journalPath string) int {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "recent", "--unreconciled"}); code != 0 {
		t.Fatalf("recent exit=%d output=%s", code, stdout.String())
	}
	var doc pagedEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Result.Total
}

func TestAwaitAnyLabelPrefixReturnsOldestUnreconciledAndAcknowledgesOnlyIt(t *testing.T) {
	journalPath := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	first := completedRun(t, journalPath, "lane.alpha")
	second := completedRun(t, journalPath, "lane.beta")
	completedRun(t, journalPath, "other.gamma")
	if total := unreconciledTotal(t, journalPath); total != 3 {
		t.Fatalf("seeded unreconciled=%d", total)
	}

	code, doc, out := awaitAny(t, journalPath, "--label-prefix", "lane.", "--timeout", "5s")
	if code != 0 || doc.Result.ExecutionID != first || doc.Result.MatchedKind != "terminal" || doc.Result.PendingMatches != 1 || doc.Result.Watched != 2 || !doc.Result.Acknowledged {
		t.Fatalf("first await-any exit=%d output=%s", code, out)
	}
	assertResultMatchesSchemaShape(t, out, "await-any-result.schema.json")
	if doc.Result.Selector["label_prefix"] != "lane." {
		t.Fatalf("selector=%v", doc.Result.Selector)
	}
	if total := unreconciledTotal(t, journalPath); total != 2 {
		t.Fatalf("await-any acknowledged more than the returned execution: unreconciled=%d", total)
	}

	code, doc, out = awaitAny(t, journalPath, "--label-prefix", "lane.", "--timeout", "5s")
	if code != 0 || doc.Result.ExecutionID != second || doc.Result.PendingMatches != 0 {
		t.Fatalf("second await-any exit=%d output=%s", code, out)
	}

	code, doc, out = awaitAny(t, journalPath, "--label-prefix", "lane.", "--timeout", "1s")
	if code != output.ExitCodeFor(output.CodeTimeout) || doc.Error.Code != "timeout" || doc.Error.Details["watched"] != float64(2) {
		t.Fatalf("drained await-any exit=%d output=%s", code, out)
	}

	code, doc, out = awaitAny(t, journalPath, "--label-prefix", "lane.", "--include-acknowledged", "--timeout", "5s")
	if code != 0 || doc.Result.ExecutionID != first {
		t.Fatalf("include-acknowledged exit=%d output=%s", code, out)
	}
}

func TestAwaitAnyExplicitExecutionsAreLevelTriggeredAndSinceSkips(t *testing.T) {
	journalPath := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	first := completedRun(t, journalPath, "")
	second := completedRun(t, journalPath, "")
	args := []string{"--execution", second, "--execution", first, "--timeout", "5s"}
	code, doc, out := awaitAny(t, journalPath, args...)
	if code != 0 || doc.Result.ExecutionID != first || doc.Result.PendingMatches != 1 {
		t.Fatalf("explicit await-any exit=%d output=%s", code, out)
	}
	// Like await, a named terminal execution matches again after collection.
	code, again, out := awaitAny(t, journalPath, args...)
	if code != 0 || again.Result.ExecutionID != first {
		t.Fatalf("repeat explicit await-any exit=%d output=%s", code, out)
	}
	code, next, out := awaitAny(t, journalPath, append(args, "--since", doc.Result.TriggerAt)...)
	if code != 0 || next.Result.ExecutionID != second {
		t.Fatalf("--since did not skip the delivered trigger: exit=%d output=%s", code, out)
	}
	code, _, out = awaitAny(t, journalPath, "--execution", first+","+second, "--since", next.Result.TriggerAt, "--timeout", "1s")
	if code != output.ExitCodeFor(output.CodeTimeout) {
		t.Fatalf("everything before --since still matched: exit=%d output=%s", code, out)
	}
}

func TestAwaitAnyWakesOnFutureLabeledExecution(t *testing.T) {
	journalPath := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	completedRun(t, journalPath, "unrelated")
	type outcome struct {
		code int
		doc  awaitAnyDocument
		out  string
	}
	done := make(chan outcome, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "await-any", "--label-prefix", "fresh", "--timeout", "30s"})
		var doc awaitAnyDocument
		_ = json.Unmarshal(stdout.Bytes(), &doc)
		done <- outcome{code: code, doc: doc, out: stdout.String()}
	}()
	time.Sleep(1500 * time.Millisecond)
	select {
	case got := <-done:
		t.Fatalf("await-any returned before a matching execution existed: %s", got.out)
	default:
	}
	launched := completedRun(t, journalPath, "fresh.lane")
	select {
	case got := <-done:
		if got.code != 0 || got.doc.Result.ExecutionID != launched {
			t.Fatalf("future execution exit=%d output=%s", got.code, got.out)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("await-any did not observe the new labeled execution")
	}
}

func TestAwaitAnyFailedTerminalUsesAwaitExitCode(t *testing.T) {
	journalPath := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	failed := runLabeled(t, journalPath, "lane.fail", "/bin/sh", "-c", "exit 3")
	code, doc, out := awaitAny(t, journalPath, "--label-prefix", "lane.", "--timeout", "5s", "--no-acknowledge")
	if code != output.ExitCodeFor(output.CodeExecutionFailed) || doc.Error.Details["execution_id"] != failed || doc.Error.Details["matched_kind"] != "terminal" || doc.Error.Details["acknowledged"] != false {
		t.Fatalf("failed await-any exit=%d output=%s", code, out)
	}
	if total := unreconciledTotal(t, journalPath); total != 1 {
		t.Fatalf("--no-acknowledge collected the failure: unreconciled=%d", total)
	}
	code, doc, out = awaitAny(t, journalPath, "--label-prefix", "lane.", "--timeout", "5s")
	if code != output.ExitCodeFor(output.CodeExecutionFailed) || doc.Error.Details["acknowledged"] != true {
		t.Fatalf("failed await-any exit=%d output=%s", code, out)
	}
	if total := unreconciledTotal(t, journalPath); total != 0 {
		t.Fatalf("returned failure was not collected: unreconciled=%d", total)
	}
}

func TestAwaitAnyAttentionAndKindFilter(t *testing.T) {
	journalPath := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	source := completedRun(t, journalPath, "lane.ask")
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := journal.GetExecution(context.Background(), mustExecutionID(t, source))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := journal.AcknowledgeExecution(context.Background(), execution.ID, store.AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	execution.ID = model.Execution{}.ID
	execution.State = model.StateAttention
	execution.Liveness = model.LivenessBlocked
	execution.TerminalAt = nil
	execution.Delegation = nil
	execution.SourceBindings = nil
	attention, _, err := journal.CreateExecution(context.Background(), execution, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	journal.Close()

	code, doc, out := awaitAny(t, journalPath, "--label-prefix", "lane.", "--timeout", "5s")
	if code != output.ExitCodeFor(output.CodeAttentionRequired) || doc.Error.Details["execution_id"] != attention.ID.String() || doc.Error.Details["matched_kind"] != "attention" {
		t.Fatalf("attention await-any exit=%d output=%s", code, out)
	}
	code, _, out = awaitAny(t, journalPath, "--label-prefix", "lane.", "--kinds", "terminal", "--timeout", "1s")
	if code != output.ExitCodeFor(output.CodeTimeout) {
		t.Fatalf("--kinds terminal matched attention: exit=%d output=%s", code, out)
	}
	trigger, _ := doc.Error.Details["trigger_at"].(string)
	code, _, out = awaitAny(t, journalPath, "--label-prefix", "lane.", "--since", trigger, "--timeout", "1s")
	if code != output.ExitCodeFor(output.CodeTimeout) {
		t.Fatalf("--since did not skip seen attention: exit=%d output=%s", code, out)
	}
}

func TestAwaitAnyUsageErrors(t *testing.T) {
	journalPath := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	id := completedRun(t, journalPath, "")
	missing, err := ids.CryptoGenerator{}.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		code output.Code
		want string
	}{
		{args: nil, code: output.CodeUsage, want: "exactly one of"},
		{args: []string{"--execution", id, "--label-prefix", "lane"}, code: output.CodeUsage, want: "exactly one of"},
		{args: []string{"--label-prefix", "Lane/"}, code: output.CodeUsage, want: "label prefix"},
		{args: []string{"--label-prefix", "lane", "--kinds", "progress"}, code: output.CodeUsage, want: "unknown await-any kind"},
		{args: []string{"--label-prefix", "lane", "--kinds", "stall"}, code: output.CodeCapabilityUnavailable, want: "stall"},
		{args: []string{"--label-prefix", "lane", "--since", "yesterday"}, code: output.CodeUsage, want: "RFC3339"},
		{args: []string{"--label-prefix", "lane", "--timeout", "1s", "--no-timeout"}, code: output.CodeUsage, want: "mutually exclusive"},
		{args: []string{"--execution", missing.String(), "--timeout", "1s"}, code: output.CodeNotFound, want: "awaited execution"},
	} {
		code, _, out := awaitAny(t, journalPath, test.args...)
		if code != output.ExitCodeFor(test.code) || !strings.Contains(out, test.want) {
			t.Fatalf("args=%v exit=%d output=%s", test.args, code, out)
		}
	}
}

// A busy journal must not hold await-any (or await) past --timeout even
// though the default busy budget for wait commands is 120s.
func TestAwaitAnyTimeoutBoundsBusyJournal(t *testing.T) {
	journalPath := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	source := completedRun(t, journalPath, "lane.busy")
	holder, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	start := time.Now()
	code, doc, out := awaitAny(t, journalPath, "--label-prefix", "lane.", "--timeout", "1s")
	if code != output.ExitCodeFor(output.CodeTimeout) || doc.Error.Code != string(output.CodeTimeout) {
		t.Fatalf("await-any under a held journal exit=%d output=%s", code, out)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("await-any ignored --timeout while the journal was busy: %s", elapsed)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	start = time.Now()
	code = a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "await", source, "--timeout", "1s"})
	if code != output.ExitCodeFor(output.CodeTimeout) {
		t.Fatalf("await under a held journal exit=%d output=%s", code, stdout.String())
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("await ignored --timeout while the journal was busy: %s", elapsed)
	}
}

// With the legacy two-attempt open (busy budget 0) a held journal still cannot
// keep await or await-any past --timeout: each lock wait is capped by the
// caller's deadline instead of bbolt's default lock timeout.
func TestAwaitTimeoutBoundsBusyJournalWithZeroBudget(t *testing.T) {
	journalPath := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	source := completedRun(t, journalPath, "lane.busy0")
	holder, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	for _, args := range [][]string{
		{"await", source, "--timeout", "1s"},
		{"await-any", "--label-prefix", "lane.", "--timeout", "1s"},
	} {
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		start := time.Now()
		code := a.run(context.Background(), append([]string{"--output", "json", "--journal", journalPath, "--journal-busy-budget", "0"}, args...))
		if code != output.ExitCodeFor(output.CodeTimeout) {
			t.Fatalf("%v under a held journal exit=%d output=%s", args, code, stdout.String())
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("%v with busy budget 0 ignored --timeout: %s", args, elapsed)
		}
	}
}
