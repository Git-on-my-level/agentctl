package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/output"
)

func changeOpenCodeFixtureVersion(t *testing.T, f delegateFixture, version string) {
	t.Helper()
	path := filepath.Join(f.root, "cursor-agent")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "opencode 2.2.6", "opencode "+version)), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeUnverifiedVersionDoesNotParkOrContinue(t *testing.T) {
	for _, failure := range []string{"", `{"type":"error","sessionID":"s1","error":{"message":"connection reset by peer"}}`} {
		f := opencodeRecoveryFixture(t, failure, true)
		changeOpenCodeFixtureVersion(t, f, "2.2.7")
		code, out := f.invoke("original-private-task", "--wait")
		id := ""
		if failure == "" {
			if code != 0 {
				t.Fatalf("delegate %d: %s", code, out)
			}
			id = resultID(t, out)
		} else {
			if code != 11 || strings.Contains(out, "paused_network") {
				t.Fatalf("unverified park %d: %s", code, out)
			}
			id = failureExecutionIDFromOutput(t, out)
		}
		_, status := f.command(t, "", "status", id)
		if !strings.Contains(status, `"name":"resume","status":"unavailable"`) {
			t.Fatalf("unverified capability: %s", status)
		}
		code, out = f.command(t, "recover", "continue", id, "--request-key", "unverified", "--prompt-stdin", "--plan")
		if code != 5 || !strings.Contains(out, "continue_opencode_version_unverified") || f.launches(t) != 1 {
			t.Fatalf("unverified continuation %d: %s", code, out)
		}
	}
}

func TestOpenCodeContinuationRechecksCurrentVersion(t *testing.T) {
	f := opencodeRecoveryFixture(t, "", true)
	_, out := f.invoke("original-private-task", "--wait")
	id := resultID(t, out)
	changeOpenCodeFixtureVersion(t, f, "2.2.7")
	code, out := f.command(t, "recover", "continue", id, "--request-key", "changed-version", "--prompt-stdin", "--plan")
	if code != 5 || !strings.Contains(out, "continue_opencode_version_unverified") || f.launches(t) != 1 {
		t.Fatalf("changed binary continued %d: %s", code, out)
	}
}

func TestOpenCodeDeletedSessionRefusesBeforeSendingPrompt(t *testing.T) {
	f := opencodeRecoveryFixture(t, "", true)
	_, out := f.invoke("original-private-task", "--wait")
	id := resultID(t, out)
	t.Setenv("OPENCODE_TEST_EXPORT_MISSING", "1")
	args := []string{"continue", id, "--request-key", "deleted-session", "--prompt-stdin"}
	code, plan := f.command(t, "private-new-instruction", append(args, "--plan")...)
	if code != 0 {
		t.Fatalf("plan %d: %s", code, plan)
	}
	if _, err := os.Stat(filepath.Join(f.root, "exports")); !os.IsNotExist(err) {
		t.Fatalf("plan read native history: %v", err)
	}
	code, out = f.command(t, "private-new-instruction", append(args, "--wait")...)
	if code != 5 || !strings.Contains(out, "continue_opencode_session_unverified") || strings.Contains(out, "s1") || f.launches(t) != 1 {
		t.Fatalf("deleted session continued %d: %s", code, out)
	}
	// A failed metadata check must not consume the continuation's key.
	t.Setenv("OPENCODE_TEST_EXPORT_MISSING", "0")
	code, out = f.command(t, "private-new-instruction", append(args, "--wait")...)
	if code != 0 || !strings.Contains(out, "resumed answer") || f.launches(t) != 2 {
		t.Fatalf("retry %d: %s", code, out)
	}
	data, err := os.ReadFile(f.journal)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-native-export") {
		t.Fatal("native export entered journal")
	}
}

func TestOpenCodeSessionCheckIsBounded(t *testing.T) {
	f := opencodeRecoveryFixture(t, "", true)
	t.Setenv("OPENCODE_TEST_EXPORT_STALL", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	problem := verifyOpenCodeSession(ctx, runOptions{argv: []string{filepath.Join(f.root, "cursor-agent")}, cwd: f.root, resumeSession: "s1"})
	if problem == nil || problem.Code != output.CodeTimeout || time.Since(started) > 2*time.Second {
		t.Fatalf("unbounded/unproven native check: %v", problem)
	}
	if _, err := os.Stat(f.count); !os.IsNotExist(err) {
		t.Fatalf("metadata check sent prompt: %v", err)
	}
}
