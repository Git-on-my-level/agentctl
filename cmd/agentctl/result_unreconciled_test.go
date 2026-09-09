package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

type unreconciledEnvelope struct {
	Result struct {
		Executions   []unreconciledResultItem `json:"executions"`
		Count        int                      `json:"count"`
		Total        int                      `json:"total"`
		HasMore      bool                     `json:"has_more"`
		Collected    int                      `json:"collected"`
		Skipped      int                      `json:"skipped"`
		Acknowledged int                      `json:"acknowledged"`
	} `json:"result"`
	Warnings []struct {
		Code string `json:"code"`
	} `json:"warnings"`
}

type pagedEnvelope struct {
	Result struct {
		Count   int  `json:"count"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	} `json:"result"`
}

// seedTerminalBacklog leaves count uncollected terminal executions behind.
func seedTerminalBacklog(t *testing.T, a *app, journalPath string, stdout *bytes.Buffer, count int, label string) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		stdout.Reset()
		native := fmt.Sprintf(`{"type":"result","status":"completed","result":"BACKLOG_%d"}`, i)
		args := []string{"--output", "json", "--journal", journalPath, "run", "--adapter", "generic-process"}
		if label != "" && i%2 == 0 {
			args = append(args, "--label", label)
		}
		args = append(args, "--", "/bin/echo", native)
		if code := a.run(context.Background(), args); code != 0 {
			t.Fatalf("seed %d exit=%d output=%s", i, code, stdout.String())
		}
		var doc struct {
			Result model.Execution `json:"result"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, doc.Result.ID.String())
	}
	return ids
}

func TestUnreconciledBacklogIsCountableAndCollectableInBulk(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	seeded := seedTerminalBacklog(t, a, journalPath, &stdout, 22, "review")

	// The backlog must be countable without paging to discover its size.
	for _, command := range [][]string{
		{"--output", "json", "--journal", journalPath, "inbox"},
		{"--output", "json", "--journal", journalPath, "recent", "--unreconciled"},
	} {
		stdout.Reset()
		if code := a.run(context.Background(), command); code != 0 {
			t.Fatalf("%v exit=%d output=%s", command, code, stdout.String())
		}
		var doc pagedEnvelope
		if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Result.Count != 20 || doc.Result.Total != 22 || !doc.Result.HasMore {
			t.Fatalf("%v count=%d total=%d has_more=%v", command, doc.Result.Count, doc.Result.Total, doc.Result.HasMore)
		}
	}

	// A bounded page reports the full backlog and collects only its own slice.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "result", "--unreconciled", "--limit", "5"}); code != 0 {
		t.Fatalf("bounded collect exit=%d output=%s", code, stdout.String())
	}
	var bounded unreconciledEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &bounded); err != nil {
		t.Fatal(err)
	}
	if bounded.Result.Count != 5 || bounded.Result.Total != 22 || !bounded.Result.HasMore || bounded.Result.Collected != 5 || bounded.Result.Acknowledged != 5 {
		t.Fatalf("bounded collect=%s", stdout.String())
	}
	for _, item := range bounded.Result.Executions {
		if item.Status != "collected" || !item.Acknowledged || item.Outcome == nil || item.Outcome.Content == nil {
			t.Fatalf("item did not carry a collected result: %#v", item)
		}
	}

	// The remainder collects in one pass and every result is delivered, not
	// merely stamped.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "result", "--unreconciled"}); code != 0 {
		t.Fatalf("collect exit=%d output=%s", code, stdout.String())
	}
	var collected unreconciledEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &collected); err != nil {
		t.Fatal(err)
	}
	if collected.Result.Count != 17 || collected.Result.Total != 17 || collected.Result.HasMore || collected.Result.Acknowledged != 17 {
		t.Fatalf("collect=%s", stdout.String())
	}
	seen := map[string]bool{}
	for _, item := range collected.Result.Executions {
		seen[item.ExecutionID] = true
	}
	for _, item := range bounded.Result.Executions {
		seen[item.ExecutionID] = true
	}
	for _, id := range seeded {
		if !seen[id] {
			t.Fatalf("execution %s was never collected", id)
		}
	}

	// Re-running is a no-op: acknowledgement is first-write-wins.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "result", "--unreconciled"}); code != 0 {
		t.Fatalf("repeat collect exit=%d output=%s", code, stdout.String())
	}
	var repeat unreconciledEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &repeat); err != nil {
		t.Fatal(err)
	}
	if repeat.Result.Count != 0 || repeat.Result.Total != 0 || repeat.Result.HasMore {
		t.Fatalf("repeat collect was not idempotent: %s", stdout.String())
	}

	// The inbox is empty because collection, not deletion, cleared it.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "inbox"}); code != 0 {
		t.Fatalf("inbox exit=%d output=%s", code, stdout.String())
	}
	var inbox pagedEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &inbox); err != nil {
		t.Fatal(err)
	}
	if inbox.Result.Total != 0 {
		t.Fatalf("inbox still reports a backlog: %s", stdout.String())
	}
}

func TestResultUnreconciledFiltersByLabel(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	seedTerminalBacklog(t, a, journalPath, &stdout, 6, "review")

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "result", "--unreconciled", "--label", "review"}); code != 0 {
		t.Fatalf("labelled collect exit=%d output=%s", code, stdout.String())
	}
	var labelled unreconciledEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &labelled); err != nil {
		t.Fatal(err)
	}
	if labelled.Result.Total != 3 || labelled.Result.Collected != 3 {
		t.Fatalf("label filter=%s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "recent", "--unreconciled"}); code != 0 {
		t.Fatalf("recent exit=%d output=%s", code, stdout.String())
	}
	var remaining pagedEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &remaining); err != nil {
		t.Fatal(err)
	}
	if remaining.Result.Total != 3 {
		t.Fatalf("unlabelled work was collected too: %s", stdout.String())
	}
}

func TestResultUnreconciledReportsConflictedEvidenceWithoutFailingTheBatch(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	seeded := seedTerminalBacklog(t, a, journalPath, &stdout, 3, "")

	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	conflicted := seeded[1]
	execution, err := journal.GetExecution(context.Background(), mustExecutionID(t, conflicted))
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
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "result", "--unreconciled"}); code != 0 {
		t.Fatalf("collect exit=%d output=%s", code, stdout.String())
	}
	var doc unreconciledEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Result.Collected != 2 || doc.Result.Skipped != 1 || doc.Result.Acknowledged != 2 {
		t.Fatalf("conflicted item aborted the batch: %s", stdout.String())
	}
	var skipped *unreconciledResultItem
	for i := range doc.Result.Executions {
		if doc.Result.Executions[i].Status == "skipped" {
			skipped = &doc.Result.Executions[i]
		}
	}
	if skipped == nil || skipped.ExecutionID != conflicted || skipped.Acknowledged || skipped.SkipReason != "unknown_state" {
		t.Fatalf("conflicted item not reported: %s", stdout.String())
	}
	warned := false
	for _, warning := range doc.Warnings {
		if warning.Code == "unreconciled_items_skipped" {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("skipped item was not surfaced as a warning: %s", stdout.String())
	}

	// Fail-closed means the conflicted execution stays visible for reconciliation.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "recent", "--unreconciled"}); code != 0 {
		t.Fatalf("recent exit=%d output=%s", code, stdout.String())
	}
	var remaining pagedEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &remaining); err != nil {
		t.Fatal(err)
	}
	if remaining.Result.Total != 1 {
		t.Fatalf("conflicted execution was silently reconciled: %s", stdout.String())
	}
}

func TestResultUnreconciledRejectsSingleResultFlags(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	for _, args := range [][]string{
		{"result", "--unreconciled", "--content"},
		{"result", "--unreconciled", "--min-result-bytes", "10"},
		{"result", "--unreconciled", "--require-result-source", "assistant"},
		{"result", "--unreconciled", "exec-one-two-three-four-five-six"},
		{"result", "exec-one-two-three-four-five-six", "--limit", "5"},
	} {
		stdout.Reset()
		full := append([]string{"--output", "json", "--journal", journalPath}, args...)
		if code := a.run(context.Background(), full); code != 2 || !strings.Contains(stdout.String(), `"code":"usage"`) {
			t.Fatalf("%v exit=%d output=%s", args, code, stdout.String())
		}
	}
}

func mustExecutionID(t *testing.T, value string) ids.ExecutionID {
	t.Helper()
	id, err := ids.ParseExecutionID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
