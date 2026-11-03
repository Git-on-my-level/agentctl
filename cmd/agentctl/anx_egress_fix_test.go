package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestANXRecoveryDryRunAbsentHasNoSideEffects(t *testing.T) {
	root := testutil.TempDir(t)
	config := writeAnxConfig(t, "/missing-anx")
	journal := filepath.Join(root, "absent", "journal.db")
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	a.startANXWorker = func(common, string) error { t.Fatal("recovery started a worker"); return nil }
	for _, args := range [][]string{{"requeue-dead", "--dry-run"}, {"requeue-dead"}} {
		out.Reset()
		argv := append([]string{"--config", config, "--journal", journal, "anx"}, args...)
		if code := a.run(context.Background(), argv); code != 0 {
			t.Fatalf("exit %d: %s", code, out.String())
		}
		if !strings.Contains(out.String(), `"requeued":0`) {
			t.Fatalf("%s", out.String())
		}
		if _, err := os.Stat(filepath.Dir(journal)); !os.IsNotExist(err) {
			t.Fatalf("recovery created absent state: %v", err)
		}
	}
	for _, args := range [][]string{{"status", "--dry-run"}, {"flush", "--dry-run"}, {"requeue-dead", "--bad"}} {
		argv := append([]string{"--config", config, "--journal", journal, "anx"}, args...)
		if code := a.run(context.Background(), argv); code != 2 {
			t.Fatalf("invalid arguments exit %d", code)
		}
	}
}

func TestANXWorkerIdentityEnvironment(t *testing.T) {
	t.Setenv("ANX_AS", "inherited-parent")
	for _, tc := range []struct{ adapter, want string }{{"claude-code", "claude"}, {"codex", "codex"}, {"multica", "multica"}} {
		count := 0
		for _, entry := range anxWorkerEnvironment(tc.adapter) {
			if strings.HasPrefix(entry, "ANX_AS=") {
				count++
				if entry != "ANX_AS="+tc.want {
					t.Fatalf("%s", entry)
				}
			}
		}
		if count != 1 {
			t.Fatalf("ANX_AS entries %d", count)
		}
	}
}
