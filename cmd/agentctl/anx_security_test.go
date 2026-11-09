package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestANXSecurityZeroConfigDefault(t *testing.T) {
	for _, configKind := range []string{"missing", "no_anx_section"} {
		t.Run(configKind, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			root := testutil.TempDir(t)
			journal := filepath.Join(root, "state", "journal.db")
			configPath := filepath.Join(root, "missing.json")
			if configKind == "no_anx_section" {
				configPath = writeAnxConfig(t, "")
			}
			id := seedTerminalLabeledExecution(t, a, journal, []string{"anx.card.dogfooding"})
			a.anxRegisterTerminal(common{configPath: configPath, journalPath: journal}, mustParseExecID(t, id))
			pending, err := openSpoolForTest(t, journal).ListPending()
			if err != nil || len(pending) != 1 {
				t.Fatalf("default-on spool missing: %v %v", pending, err)
			}
		})
	}
}

func TestANXSecurityTerminalLatencyUnreachable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	root := testutil.TempDir(t)
	journal := filepath.Join(root, "state", "journal.db")
	script := filepath.Join(root, "unreachable-anx")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := writeAnxConfig(t, script)
	id := seedTerminalLabeledExecution(t, a, journal, []string{"anx.card.dogfooding"})
	started := time.Now()
	a.anxRegisterTerminal(common{configPath: configPath, journalPath: journal}, mustParseExecID(t, id))
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("terminal return waited on unreachable ANX: %s", elapsed)
	}
}

// Exercise the shipped executable: pipes close promptly, the worker survives
// its parent, and an unreachable ANX remains queued with a classified retry.
func TestANXSecurityDetachedCommandLatency(t *testing.T) {
	root := testutil.TempDir(t)
	binary := filepath.Join(root, "agentctl")
	if output, err := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	release := filepath.Join(root, "release-anx")
	// ANX cannot respond until the caller explicitly releases it. Its own
	// 60-second timeout exceeds the command's 30-second deadline, so joining
	// transport I/O cannot masquerade as a successful detached return.
	defer func() { _ = os.WriteFile(release, nil, 0600) }()
	script := filepath.Join(root, "unreachable-anx")
	body := "#!/bin/sh\nwhile [ ! -f '" + release + "' ]; do sleep 0.05; done\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := writeAnxConfig(t, script)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["anx"].(map[string]any)["run_links"].(map[string]any)["flush_timeout_seconds"] = 60
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	journal := filepath.Join(root, "state", "journal.db")
	command := exec.CommandContext(ctx, binary, "--config", configPath, "--journal", journal, "run", "--adapter", "generic-process", "--anx-card", "dogfooding", "--", "/bin/echo", `{"type":"result","session_id":"fixture-session","status":"completed","result":"done"}`)
	// Measure detached ANX behavior without managed-Git discovery in the
	// caller's repository contributing to this wall-clock assertion.
	command.Dir = root
	command.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off")
	started := time.Now()
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run: %v %s", err, out)
	}
	latency := time.Since(started)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := openSpoolForTest(t, journal).ListPending()
		if err != nil {
			t.Fatal(err)
		}
		// Start and terminal each schedule a worker. Observing only the first
		// retry lets TempDir cleanup race the worker writing the other phase.
		allRetried := len(pending) == 2
		for _, entry := range pending {
			if entry.Attempts == 0 {
				allRetried = false
			} else if entry.LastError != "anx_protocol_error" {
				t.Fatalf("unexpected error class: %s", entry.LastError)
			}
		}
		if allRetried {
			waitForDetachedANXWorkers(t, journal)
			t.Logf("command returned in %s; detached retries recorded", latency)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("detached flush never ran")
}

// The detached flush worker outlives the command that started it, and its
// last spool writes can land after the recorded attempts are visible. Wait for
// it to exit so the test's TempDir cleanup cannot race those writes.
func waitForDetachedANXWorkers(t *testing.T, journal string) {
	t.Helper()
	pattern := "_anx-flush-worker.*" + regexp.QuoteMeta(journal)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("pgrep", "-f", pattern).Output()
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			// pgrep exits 1 when no process matches.
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("detached ANX flush worker still running after the test observed its retries")
}

func TestANXSecurityStatusErrorsAreEnums(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	root := testutil.TempDir(t)
	journal := filepath.Join(root, "state", "journal.db")
	configPath := writeAnxConfig(t, filepath.Join(root, "secret-binary-path"))
	id := seedTerminalLabeledExecution(t, a, journal, []string{"anx.card.dogfooding"})
	a.anxRegisterTerminal(common{configPath: configPath, journalPath: journal}, mustParseExecID(t, id))
	spool := openSpoolForTest(t, journal)
	entries, _ := spool.ListPending()
	entries[0].LastError = "/Users/private secret-value"
	raw, _ := json.Marshal(entries[0])
	path := filepath.Join(spool.Root(), "pending", entries[0].ExecutionID+"."+string(entries[0].Phase)+".json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journal, "anx", "status"}); code != 0 {
		t.Fatal("status failed")
	}
	if strings.Contains(stdout.String(), "secret-value") || strings.Contains(stdout.String(), "/Users/private") {
		t.Fatal("legacy error text escaped in status")
	}
}
