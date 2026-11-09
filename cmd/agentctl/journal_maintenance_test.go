package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestSupervisorRetentionDefaultAndOptOut(t *testing.T) {
	for _, mode := range []string{"default", "flag", "environment"} {
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
					return ""
				}
				return getenv(key)
			}
			args := []string{"--journal", path, "supervisor", "run", "--once"}
			if mode == "flag" {
				args = append(args, "--no-journal-retention")
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
			if mode == "default" {
				want = 0
			}
			if len(rows) != want {
				t.Fatalf("remaining=%d want=%d stderr=%s", len(rows), want, stderr.String())
			}
		})
	}
}
