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

	"github.com/Git-on-my-level/agentctl/internal/contracts"
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
