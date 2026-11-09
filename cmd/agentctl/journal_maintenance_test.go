package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/anxrun"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
	"go.etcd.io/bbolt"
)

func TestSupervisorRetentionDefaultAndOptOut(t *testing.T) {
	for _, mode := range []string{"default", "flag", "environment", "immediate_flag", "immediate_environment"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
			var out, stderr bytes.Buffer
			a := testApp(&out, &stderr)
			ctx := context.Background()
			if code := a.run(ctx, []string{"--journal", path, "run", "--adapter", "generic-process", "--", "/bin/echo", "test result"}); code != 0 {
				t.Fatalf("seed=%d %s", code, out.String())
			}
			out.Reset()
			if code := a.run(ctx, []string{"--journal", path, "result", "--unreconciled"}); code != 0 {
				t.Fatalf("collect=%d %s", code, out.String())
			}
			out.Reset()
			// Advance past retention age and the startup grace without a real wait.
			base := time.Now().Add(32 * 24 * time.Hour)
			calls := 0
			a.now = func() time.Time { calls++; return base.Add(time.Duration(calls) * 2 * time.Minute) }
			getenv := a.getenv
			a.getenv = func(key string) string {
				if key == "AGENTCTL_JOURNAL_RETENTION" {
					if mode == "environment" {
						return "off"
					}
					if mode == "immediate_environment" {
						return "immediate"
					}
					return ""
				}
				return getenv(key)
			}
			args := []string{"--journal", path, "supervisor", "run", "--once"}
			if mode == "flag" {
				args = append(args, "--no-journal-retention")
			}
			if mode == "immediate_flag" {
				args = append(args, "--journal-retention-immediate")
			}
			if code := a.run(ctx, args); code != 0 {
				t.Fatalf("once=%d %s %s", code, out.String(), stderr.String())
			}
			j, err := store.Open(path, store.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			rows, err := j.ListExecutions(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "default" || mode == "immediate_flag" || mode == "immediate_environment" {
				want = 0
			}
			if len(rows) != want {
				t.Fatalf("remaining=%d want=%d stderr=%s", len(rows), want, stderr.String())
			}
		})
	}
}

func TestLegacyRetentionPreviewThenAutomaticEnforcement(t *testing.T) {
	for _, immediate := range []bool{false, true} {
		t.Run(fmt.Sprint("immediate=", immediate), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
			var out, stderr bytes.Buffer
			a := testApp(&out, &stderr)
			id := mustParseExecID(t, seedTerminalLabeledExecution(t, a, path, nil))
			j, err := store.Open(path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			row, err := j.GetExecution(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := j.AcknowledgeExecution(ctx, id, store.AcknowledgementResult); err != nil {
				t.Fatal(err)
			}
			j.Close()
			// Remove only the new key, as if the previous release last wrote this DB.
			db, err := bbolt.Open(path, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("metadata")).Delete([]byte("retention_enforce_at")) })
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			start := row.TerminalAt.Add(32 * 24 * time.Hour)
			count, err := maintainJournal(ctx, path, start, immediate, nil, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			if immediate {
				if count != 1 {
					t.Fatalf("immediate removed=%d", count)
				}
				return
			}
			if count != 0 {
				t.Fatalf("preview deleted %d executions", count)
			}
			reportPath := path + ".retention-report.json"
			raw, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			var report journalRetentionReport
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatal(err)
			}
			if report.Mode != "plan_only" || report.Records.Executions != 1 || report.Records.Outcomes != 1 || report.Records.Events == 0 || report.Oldest == nil || report.Newest == nil || !report.Oldest.Equal(*row.TerminalAt) || !report.Newest.Equal(*row.TerminalAt) || !report.EnforceAt.Equal(start.Add(store.RetentionPreviewWindow)) {
				t.Fatalf("report=%s", raw)
			}
			if bytes.Contains(raw, []byte(id.String())) || bytes.Contains(raw, []byte("Landed the watcher")) {
				t.Fatalf("report leaks content: %s", raw)
			}
			info, err := os.Stat(reportPath)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("report mode=%v %v", info, err)
			}
			// Subsequent independent passes neither reset the window nor delete early.
			count, err = maintainJournal(ctx, path, start.Add(store.RetentionPreviewWindow-time.Second), false, nil, &stderr)
			if err != nil || count != 0 {
				t.Fatalf("last preview=%d %v", count, err)
			}
			reader, err := store.Open(path, store.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			_, outcomeErr := reader.GetOutcome(ctx, id)
			events, eventErr := reader.ListEvents(ctx, id, contracts.EventQuery{})
			reader.Close()
			if outcomeErr != nil || eventErr != nil || len(events) == 0 {
				t.Fatalf("preview removed history: %v %v", outcomeErr, eventErr)
			}
			count, err = maintainJournal(ctx, path, start.Add(store.RetentionPreviewWindow), false, nil, &stderr)
			if err != nil || count != 1 {
				t.Fatalf("automatic enforcement=%d %v", count, err)
			}
		})
	}
}

func TestLegacyANXSpoolRootChangeRetainsCollectedUnlabeledHistory(t *testing.T) {
	ctx := context.Background()
	root := testutil.TempDir(t)
	path := filepath.Join(root, "state", "journal.db")
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	legacyID := mustParseExecID(t, seedTerminalLabeledExecution(t, a, path, nil))
	controlID := mustParseExecID(t, seedTerminalLabeledExecution(t, a, path, nil))
	j, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := j.GetExecution(ctx, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []ids.ExecutionID{legacyID, controlID} {
		if _, _, err := j.AcknowledgeExecution(ctx, id, store.AcknowledgementResult); err != nil {
			t.Fatal(err)
		}
	}
	j.Close()
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("retention_tracking")).Delete([]byte(legacyID.String()))
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := filepath.Join(root, "old-custom-links")
	oldSpool, err := anxrun.OpenSpool(oldRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy delivered link whose original custom root is no longer
	// in configuration. A durable replay index survives pending-file deletion.
	key := "agentctl://" + legacy.OriginHostID.String() + "/" + legacyID.String() + "/terminal"
	index, err := json.Marshal(map[string]any{"counters": map[string]int{}, "assigned": map[string]int{key: 1}, "delivered": map[string]int{key: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "sequence-index.json"), index, 0600); err != nil {
		t.Fatal(err)
	}
	prior, err := oldSpool.LinkedExecutions()
	if err != nil || len(prior) != 1 || prior[0] != legacyID {
		t.Fatalf("prior links=%v %v", prior, err)
	}
	configPath := filepath.Join(root, "config.json")
	config, err := json.Marshal(map[string]any{"schema_version": 1, "anx": map[string]any{"run_links": map[string]any{"default": "all", "spool_dir": filepath.Join(root, "new-custom-links")}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	links, err := a.anxRetentionLinks(common{journalPath: path, configPath: configPath})
	if err != nil || len(links) != 0 {
		t.Fatalf("current roots unexpectedly establish legacy absence: %v %v", links, err)
	}
	now := legacy.TerminalAt.Add(60 * 24 * time.Hour)
	removed, err := maintainJournal(ctx, path, now, true, links, &stderr)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d want tracked control only; err=%v", removed, err)
	}
	j, err = store.Open(path, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.GetOutcome(ctx, legacyID); err != nil {
		t.Fatal("legacy link lost its outcome", err)
	}
	events, err := j.ListEvents(ctx, legacyID, contracts.EventQuery{})
	if err != nil || len(events) == 0 {
		t.Fatalf("legacy events=%d %v", len(events), err)
	}
	plan, err := j.PlanCleanup(ctx, now, store.CleanupOptions{IncludeUnreconciled: true})
	if err != nil || len(plan.Eligible) != 0 || len(plan.Protected) != 1 || plan.Protected[0].Reasons[0] != "legacy_anx_history_unknown" {
		t.Fatalf("unknown legacy history not protected: %+v %v", plan, err)
	}
}
