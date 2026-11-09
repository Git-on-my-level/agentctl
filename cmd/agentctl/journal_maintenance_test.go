package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
	for _, e := range []model.Execution{native, remote} {
		if _, err := read.GetExecution(context.Background(), e.ID); err != store.ErrNotFound {
			t.Fatalf("old acknowledged %s retained: %v", e.Authority, err)
		}
	}
	for _, e := range []model.Execution{recent, unread} {
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
