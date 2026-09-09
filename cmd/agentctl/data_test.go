package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestDataInventoryAndCleanupRequireExplicitReviewedApply(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", "retention result"}); code != 0 {
		t.Fatalf("seed exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "data", "inventory"}); code != 0 {
		t.Fatalf("inventory exit=%d output=%s", code, stdout.String())
	}
	var inventory struct {
		Result store.RetentionInventory `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.Result.Executions.Terminal != 1 || inventory.Result.Executions.Outcomes != 1 {
		t.Fatalf("unexpected inventory: %s", stdout.String())
	}
	// Cleanup protects uncollected results, so collect the seeded backlog first
	// and leave this case to assert the reviewed-digest contract.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "result", "--unreconciled"}); code != 0 {
		t.Fatalf("collect exit=%d output=%s", code, stdout.String())
	}
	cutoff := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "data", "cleanup", "--before", cutoff, "--apply"}); code != 2 || !strings.Contains(stdout.String(), "plan-digest") {
		t.Fatalf("unreviewed apply exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "data", "cleanup", "--before", cutoff, "--plan"}); code != 0 {
		t.Fatalf("plan exit=%d output=%s", code, stdout.String())
	}
	var planned struct {
		Result store.CleanupPlan `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.Result.Applied || len(planned.Result.Eligible) != 1 || planned.Result.PlanDigest == "" {
		t.Fatalf("unexpected plan: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "data", "cleanup", "--before", cutoff, "--apply", "--plan-digest", planned.Result.PlanDigest}); code != 0 {
		t.Fatalf("apply exit=%d output=%s", code, stdout.String())
	}
	var applied struct {
		Result store.CleanupPlan `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if !applied.Result.Applied || len(applied.Result.Eligible) != 1 {
		t.Fatalf("unexpected applied plan: %s", stdout.String())
	}
}

func TestDataCleanupPlanDoesNotCreateMissingJournal(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "missing", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	cutoff := time.Now().UTC().Format(time.RFC3339)
	if code := a.run(context.Background(), []string{"--journal", journalPath, "data", "cleanup", "--before", cutoff, "--plan"}); code != 3 {
		t.Fatalf("missing plan exit=%d output=%s", code, stdout.String())
	}
}

func TestDataCleanupProtectsUncollectedResultsUntilExplicitOptOut(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", "uncollected result"}); code != 0 {
		t.Fatalf("seed exit=%d output=%s", code, stdout.String())
	}
	cutoff := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "data", "cleanup", "--before", cutoff, "--plan"}); code != 0 {
		t.Fatalf("plan exit=%d output=%s", code, stdout.String())
	}
	var protective struct {
		Result store.CleanupPlan `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &protective); err != nil {
		t.Fatal(err)
	}
	if len(protective.Result.Eligible) != 0 || protective.Result.ProtectedUnreconciled != 1 || protective.Result.IncludeUnreconciled {
		t.Fatalf("uncollected terminal was not protected: %s", stdout.String())
	}
	if len(protective.Result.Protected) != 1 || protective.Result.Protected[0].Reasons[0] != "result_unreconciled" {
		t.Fatalf("unexpected protection reason: %s", stdout.String())
	}

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "data", "cleanup", "--before", cutoff, "--include-unreconciled", "--plan"}); code != 0 {
		t.Fatalf("opt-out plan exit=%d output=%s", code, stdout.String())
	}
	var permissive struct {
		Result store.CleanupPlan `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &permissive); err != nil {
		t.Fatal(err)
	}
	if len(permissive.Result.Eligible) != 1 || !permissive.Result.IncludeUnreconciled {
		t.Fatalf("opt-out plan did not select the uncollected terminal: %s", stdout.String())
	}
	if permissive.Result.PlanDigest == protective.Result.PlanDigest {
		t.Fatalf("opting in did not change the plan digest: %s", stdout.String())
	}

	// The reviewed digest is bound to the policy it was produced under.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "data", "cleanup", "--before", cutoff, "--apply", "--plan-digest", permissive.Result.PlanDigest}); code == 0 {
		t.Fatalf("permissive digest applied without the flag: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "data", "cleanup", "--before", cutoff, "--include-unreconciled", "--apply", "--plan-digest", permissive.Result.PlanDigest}); code != 0 {
		t.Fatalf("opt-out apply exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "uncollected_results_included") {
		t.Fatalf("opt-out apply did not warn about deleting uncollected results: %s", stdout.String())
	}
}
