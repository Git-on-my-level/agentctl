package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestJournalMaintenanceDefaultCutoffKillSwitchAndBothAuthorities(t *testing.T) {
	root := testutil.TempDir(t)
	path := filepath.Join(root, "state", "journal.db")
	cfgPath := filepath.Join(root, "config.json")
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	now := a.now().UTC()
	j, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	native := mustCreateTerminal(t, j, "codex", model.StateCompleted, now.Add(-15*24*time.Hour))
	remote := mustCreateMultica(t, j, model.StateCompleted, now.Add(-15*24*time.Hour), true)
	recent := mustCreateTerminal(t, j, "codex", model.StateCompleted, now.Add(-13*24*time.Hour))
	unread := mustCreateTerminal(t, j, "codex", model.StateCompleted, now.Add(-30*24*time.Hour))
	for _, e := range []model.Execution{native, remote, recent} {
		if _, _, err := j.AcknowledgeExecution(context.Background(), e.ID, store.AcknowledgementResult); err != nil {
			t.Fatal(err)
		}
	}
	j.Close()
	off := false
	cfg := config.Config{SchemaVersion: 1, Journal: &config.Journal{Retention: &config.JournalRetention{Enabled: &off}}}
	if err := config.Save(cfgPath, cfg, false); err != nil {
		t.Fatal(err)
	}
	c := common{configPath: cfgPath, journalPath: path}
	if err := a.maintainJournal(context.Background(), c, path); err != nil {
		t.Fatal(err)
	}
	read, err := store.Open(path, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := read.GetExecution(context.Background(), native.ID); err != nil {
		t.Fatal("disabled retention deleted", err)
	}
	read.Close()
	// The same supervisor observes a policy edit on the next cycle.
	cfg.Journal = nil
	if err := config.Save(cfgPath, cfg, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.maintainJournal(context.Background(), c, path); err != nil {
			t.Fatal(err)
		}
	}
	read, err = store.Open(path, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	for _, e := range []model.Execution{native} {
		if _, err := read.GetExecution(context.Background(), e.ID); err != store.ErrNotFound {
			t.Fatalf("old acknowledged %s retained: %v", e.Authority, err)
		}
	}
	gotRemote, err := read.GetExecution(context.Background(), remote.ID)
	if err != nil || len(gotRemote.SourceBindings) != len(remote.SourceBindings) || gotRemote.SourceBindings[0].Fingerprint != remote.SourceBindings[0].Fingerprint {
		t.Fatalf("Multica replay binding lost: %+v %v", gotRemote, err)
	}
	for _, e := range []model.Execution{remote, recent, unread} {
		if _, err := read.GetExecution(context.Background(), e.ID); err != nil {
			t.Fatalf("protected %s removed: %v", e.ID, err)
		}
	}
}

func TestJournalMaintenanceMissingJournalDoesNotCreate(t *testing.T) {
	root := testutil.TempDir(t)
	path := filepath.Join(root, "absent", "journal.db")
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	if err := a.maintainJournal(context.Background(), common{configPath: filepath.Join(root, "missing.json")}, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("maintenance created state", err)
	}
}

func TestJournalMaintenanceTimeoutIsLoggedAndRateLimited(t *testing.T) {
	root := testutil.TempDir(t)
	path := filepath.Join(root, "state", "journal.db")
	j, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	now := a.now()
	a.now = func() time.Time { return now }
	c := common{configPath: filepath.Join(root, "missing.json"), journalPath: path}
	m := &journalMaintenance{}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := m.run(ctx, a, c, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout swallowed: %v", err)
	}
	if !strings.Contains(stderr.String(), "retention deferred:") || !strings.Contains(stderr.String(), "deadline exceeded") {
		t.Fatal(stderr.String())
	}
	size := stderr.Len()
	// A skipped cadence tick preserves the failure in health but does no scan.
	if err := m.run(context.Background(), a, c, path); !errors.Is(err, context.DeadlineExceeded) || stderr.Len() != size {
		t.Fatalf("cadence cleared failure or re-ran: %v %s", err, stderr.String())
	}
	now = now.Add(5 * time.Minute)
	if err := m.run(context.Background(), a, c, path); err != nil {
		t.Fatal(err)
	}
	if m.lastError != nil {
		t.Fatal("successful pass did not clear failure")
	}
}

func TestPendingOutboxScanUsesReadOnlyHandle(t *testing.T) {
	root := testutil.TempDir(t)
	path := filepath.Join(root, "state", "journal.db")
	j, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	reader, err := store.Open(path, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	items, err := (journalSupervisorOutbox{path: path}).ListPending(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("pending read contended with reader: %v %v", items, err)
	}
}
