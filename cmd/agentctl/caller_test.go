package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func enableCallerEnvironment(a *app) {
	base := a.getenv
	a.getenv = func(key string) string {
		if key == adapter.CallerHarnessEnv {
			return os.Getenv(key)
		}
		return base(key)
	}
}

func TestExecutionCallerUsesOnlyExplicitBoundedDeclaration(t *testing.T) {
	for _, value := range []string{"", "hermes", "claude-code", "codex", "cursor", "omp", "zcode", "devin", "other", "CODEX", " codex", "codex\n", "private-session-token"} {
		t.Run(value, func(t *testing.T) {
			a := &app{getenv: func(key string) string {
				if key == adapter.CallerHarnessEnv {
					return value
				}
				// Competing identity hints must not affect admission attribution.
				return "private-native-session"
			}}
			caller, problem := a.executionCaller()
			switch value {
			case "":
				if caller != nil || problem != nil {
					t.Fatalf("absent declaration caller=%#v problem=%v", caller, problem)
				}
			case "hermes", "claude-code", "codex", "cursor", "omp", "zcode", "devin", "other":
				if problem != nil || caller == nil || string(caller.Harness) != value || caller.Provenance != model.CallerDeclared {
					t.Fatalf("caller=%#v problem=%v", caller, problem)
				}
			default:
				if problem == nil || problem.Code != output.CodeUsage || caller != nil {
					t.Fatalf("invalid declaration caller=%#v problem=%v", caller, problem)
				}
				data, _ := json.Marshal(problem)
				if value == "private-session-token" && bytes.Contains(data, []byte(value)) {
					t.Fatalf("invalid declaration leaked: %s", data)
				}
			}
		})
	}
}

func TestRunCallerAdmissionReplayAndChildIsolation(t *testing.T) {
	t.Setenv(adapter.CallerHarnessEnv, "hermes")
	root := t.TempDir()
	journal := filepath.Join(root, "state", "journal.db")
	capture := filepath.Join(root, "launches")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	enableCallerEnvironment(a)
	args := []string{"--journal", journal, "run", "--adapter", "generic-process", "--idempotency-key", "caller-replay", "--", "/bin/sh", "-c", `printf '%s\n' "${AGENTCTL_CALLER_HARNESS-unset}" >> "$1"; printf '%s\n' '{"type":"result","status":"completed","result":"caller result"}'`, "_", capture}
	var first, retry struct {
		Result model.Execution `json:"result"`
	}
	for i, doc := range []*struct {
		Result model.Execution `json:"result"`
	}{&first, &retry} {
		if i == 1 {
			t.Setenv(adapter.CallerHarnessEnv, "codex")
		}
		stdout.Reset()
		if code := a.run(context.Background(), args); code != 0 {
			t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
		}
		if err := json.Unmarshal(stdout.Bytes(), doc); err != nil {
			t.Fatal(err)
		}
		if doc.Result.Caller == nil || doc.Result.Caller.Harness != model.CallerHermes || doc.Result.Caller.Provenance != model.CallerDeclared {
			t.Fatalf("caller admission=%#v", doc.Result.Caller)
		}
	}
	if first.Result.ID != retry.Result.ID {
		t.Fatal("caller metadata changed replay identity")
	}
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != "unset\n" {
		t.Fatalf("child caller or launch count=%q err=%v", got, err)
	}
	// Retrieval observes the stored declaration and does not validate or infer
	// a new caller from this invocation's environment.
	t.Setenv(adapter.CallerHarnessEnv, "private-invalid-caller-token")
	for _, command := range []string{"status", "result"} {
		stdout.Reset()
		if code := a.run(context.Background(), []string{"--journal", journal, command, first.Result.ID.String()}); code != 0 {
			t.Fatalf("%s exit=%d %s", command, code, stdout.String())
		}
		if !strings.Contains(stdout.String(), `"caller":{"harness":"hermes","provenance":"caller_declared"}`) {
			t.Fatalf("%s lost caller: %s", command, stdout.String())
		}
	}
}

func TestInvalidCallerFailsBeforeNativeProbeOrJournal(t *testing.T) {
	t.Setenv(adapter.CallerHarnessEnv, "private-session-token")
	root := t.TempDir()
	journal := filepath.Join(root, "state", "journal.db")
	capture := filepath.Join(root, "probe-or-launch")
	fixture := filepath.Join(root, "fixture")
	if err := os.WriteFile(fixture, []byte("#!/bin/sh\nprintf '%s' touched > '"+capture+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	enableCallerEnvironment(a)
	code := a.run(context.Background(), []string{"--journal", journal, "run", "--adapter", "generic-process", "--no-store-result", "--", fixture})
	if code != output.ExitCodeFor(output.CodeUsage) || !strings.Contains(stdout.String(), "caller_harness_invalid") || strings.Contains(stdout.String(), "private-session-token") {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	for _, path := range []string{journal, capture} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("invalid caller touched %s: %v", path, err)
		}
	}
}

func TestUndeclaredRunCallerStaysUnknown(t *testing.T) {
	t.Setenv(adapter.CallerHarnessEnv, "")
	t.Setenv("CODEX_THREAD_ID", "private-native-session-token")
	t.Setenv("CLAUDE_SESSION_ID", "different-native-session-token")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	enableCallerEnvironment(a)
	journal := filepath.Join(t.TempDir(), "state", "journal.db")
	code := a.run(context.Background(), []string{"--journal", journal, "run", "--adapter", "generic-process", "--", "/bin/echo", `{"type":"result","status":"completed","result":"answer"}`})
	if code != 0 || strings.Contains(stdout.String(), `"caller"`) || strings.Contains(stdout.String(), "native-session-token") {
		t.Fatalf("undeclared caller exit=%d output=%s", code, stdout.String())
	}
}

func TestDispatchCallerAdmissionPlanReplayAndEnvironmentIsolation(t *testing.T) {
	t.Setenv(adapter.CallerHarnessEnv, "hermes")
	root := t.TempDir()
	agents := `[{"id":"agent-fixture","name":"Fixture Agent","model":"fixture-model","runtime_id":"runtime-fixture","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-fixture","custom_name":"Fixture Host","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	script, err := os.ReadFile(fake)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.Replace(script, []byte("set -eu\n"), []byte("set -eu\nprintf '%s\\n' \"${AGENTCTL_CALLER_HARNESS-unset}\" >> \"$AGENTCTL_TEST_CAPTURE.caller\"\n"), 1)
	if err := os.WriteFile(fake, script, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	journal := filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "fixture-model", "speed": "regular", "use_for": "alias:fixture"}},
		map[string]any{"fixture": "fixture-host"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	enableCallerEnvironment(a)
	a.stdinIsTerminal = func() bool { return false }
	args := []string{"--config", configPath, "--journal", journal, "dispatch", "--route", "fixture fixture", "--title", "Caller admission", "--prompt-stdin", "--idempotency-key", "caller-dispatch"}
	a.stdin = strings.NewReader("private-dispatch-prompt-token")
	if code := a.run(context.Background(), append(append([]string(nil), args...), "--plan")); code != 0 {
		t.Fatalf("dispatch plan exit=%d output=%s", code, stdout.String())
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("caller plan created journal: %v", err)
	}
	var firstID string
	for i := 0; i < 2; i++ {
		if i == 1 {
			t.Setenv(adapter.CallerHarnessEnv, "cursor")
		}
		stdout.Reset()
		a.stdin = strings.NewReader("private-dispatch-prompt-token")
		if code := a.run(context.Background(), args); code != 0 {
			t.Fatalf("dispatch exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
		}
		var doc struct {
			Result struct {
				Execution model.Execution `json:"execution"`
				Reused    bool            `json:"reused"`
			} `json:"result"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Result.Execution.Caller == nil || doc.Result.Execution.Caller.Harness != model.CallerHermes || doc.Result.Execution.Caller.Provenance != model.CallerDeclared {
			t.Fatalf("dispatch caller=%#v", doc.Result.Execution.Caller)
		}
		if i == 0 {
			firstID = doc.Result.Execution.ID.String()
		} else if doc.Result.Execution.ID.String() != firstID || !doc.Result.Reused {
			t.Fatalf("dispatch caller changed replay: %s", stdout.String())
		}
	}
	seen, err := os.ReadFile(capture + ".caller")
	if err != nil || strings.TrimSpace(string(seen)) == "" {
		t.Fatalf("Multica environment capture=%q err=%v", seen, err)
	}
	for _, line := range strings.Fields(string(seen)) {
		if line != "unset" {
			t.Fatalf("Multica inherited caller declaration: %q", seen)
		}
	}
	raw, err := os.ReadFile(journal)
	if err != nil || bytes.Contains(raw, []byte("private-dispatch-prompt-token")) {
		t.Fatalf("prompt retention or journal read error: %v", err)
	}
	// Invalid declaration must fail before any Multica query or journal opens.
	t.Setenv(adapter.CallerHarnessEnv, "private-invalid-caller-token")
	stdout.Reset()
	if code := a.run(context.Background(), args); code != output.ExitCodeFor(output.CodeUsage) || strings.Contains(stdout.String(), "private-invalid-caller-token") {
		t.Fatalf("invalid dispatch caller exit=%d output=%s", code, stdout.String())
	}
	after, err := os.ReadFile(capture + ".caller")
	if err != nil || !bytes.Equal(after, seen) {
		t.Fatalf("invalid caller invoked Multica: err=%v", err)
	}
}

func TestDelegateAndContinueCallerAdmissionsRemainIndependent(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	enableCallerEnvironment(a)
	a.stdinIsTerminal = func() bool { return false }
	invoke := func(prompt string, args ...string) string {
		t.Helper()
		stdout.Reset()
		a.stdin = strings.NewReader(prompt)
		if code := a.run(context.Background(), append([]string{"--config", f.config, "--journal", f.journal}, args...)); code != 0 {
			t.Fatalf("exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
		}
		return stdout.String()
	}
	t.Setenv(adapter.CallerHarnessEnv, "hermes")
	first := resultID(t, invoke("first", "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--wait"))
	t.Setenv(adapter.CallerHarnessEnv, "cursor")
	second := resultID(t, invoke("second", "continue", first, "--request-key", "caller-turn-2", "--prompt-stdin", "--wait"))
	t.Setenv(adapter.CallerHarnessEnv, "codex")
	retry := resultID(t, invoke("second", "continue", first, "--request-key", "caller-turn-2", "--prompt-stdin", "--wait"))
	if retry != second {
		t.Fatal("caller declaration changed continue replay")
	}
	for _, tc := range []struct{ id, caller string }{{first, "hermes"}, {second, "cursor"}} {
		out := invoke("", "status", tc.id)
		if !strings.Contains(out, `"caller":{"harness":"`+tc.caller+`","provenance":"caller_declared"}`) {
			t.Fatalf("turn caller lost: %s", out)
		}
	}
	if f.launches(t) != 2 {
		t.Fatalf("continue replay launched another turn: %d", f.launches(t))
	}
}
