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
