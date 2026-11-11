package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func recoveryFixture(t *testing.T, failure string) delegateFixture {
	t.Helper()
	body := `case " $* " in
*" resume "*" thread-1 "*)
 printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}' '{"type":"item.completed","item":{"type":"agent_message","text":"resumed answer"}}' '{"type":"turn.completed"}' ;;
*)
 printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}' '` + failure + `' ;;
esac`
	f := newDelegateFixture(t, body)
	cfg := config.Config{SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{"test": {
		Adapters:         map[string]config.Adapter{"codex": {Executable: filepath.Join(f.root, "cursor-agent")}},
		AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: "codex", Family: "sol", Model: "test-model", Default: true}}},
	}}}
	if err := config.Save(f.config, cfg, true); err != nil {
		t.Fatal(err)
	}
	f.selectRequest(t, `{"family":"sol"}`)
	return f
}

func TestZCodeRepeatedParkingSettlesAncestorsAndCancels(t *testing.T) {
	f := carryFixture(t, "zcode")
	bin := filepath.Join(f.root, "cursor-agent")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 0.17.0; exit 0; fi\necho launch >> '" + f.count + "'\n" + `case " $* " in
*" --resume s1 "*)
 case " $* " in
 *" park-again "*) printf '%s\n' '{"sessionId":"s1","error":{"message":"connect ECONNRESET"}}';;
 *" unknown-failure "*) printf '%s\n' '{"sessionId":"s1","error":{"message":"unknown provider error"}}';;
 *) printf '%s\n' '{"sessionId":"s1","turnId":"t3","response":"recovered answer"}';;
 esac;;
*) printf '%s\n' '{"sessionId":"s1","error":{"message":"connect ECONNRESET"}}';;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	code, out := f.invoke("task", "--wait")
	if code != 10 {
		t.Fatalf("initial park exit=%d: %s", code, out)
	}
	first := failureExecutionIDFromOutput(t, out)
	code, out = f.command(t, "unknown-failure", "continue", first, "--request-key", "intermediate-failure", "--prompt-stdin", "--wait")
	if code != 11 {
		t.Fatalf("intermediate failure exit=%d: %s", code, out)
	}
	middle := failureExecutionIDFromOutput(t, out)
	code, out = f.command(t, "park-again", "continue", middle, "--request-key", "second-park", "--prompt-stdin", "--wait")
	if code != 10 {
		t.Fatalf("second park exit=%d: %s", code, out)
	}
	second := failureExecutionIDFromOutput(t, out)
	code, refused := f.command(t, "recover", "continue", first, "--request-key", "stale-source", "--prompt-stdin", "--plan")
	if code != 8 || !strings.Contains(refused, "continue_not_latest") || !strings.Contains(refused, second) {
		t.Fatalf("stale: %s", refused)
	}
	code, out = f.command(t, "recover", "continue", second, "--request-key", "final-recovery", "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(out, "recovered answer") || f.launches(t) != 4 {
		t.Fatalf("recover exit=%d: %s", code, out)
	}
	for _, id := range []string{first, second} {
		_, status := f.command(t, "", "status", id)
		if !strings.Contains(status, `"state":"cancelled"`) || !strings.Contains(status, "superseded_by") {
			t.Fatalf("unsettled ancestor: %s", status)
		}
	}

	g := recoveryFixture(t, `{"type":"turn.failed","error":{"message":"connection reset"}}`)
	_, out = g.invoke("task", "--wait")
	id := failureExecutionIDFromOutput(t, out)
	code, cancelled := g.command(t, "", "cancel", id)
	if code != 0 || !strings.Contains(cancelled, `"state":"cancelled"`) || g.launches(t) != 1 {
		t.Fatalf("cancel exit=%d: %s", code, cancelled)
	}
	code, refused = g.command(t, "recover", "continue", id, "--request-key", "after-cancel", "--prompt-stdin", "--plan")
	if code == 0 || !strings.Contains(refused, "continue_source_not_completed") {
		t.Fatalf("cancel resumed: %s", refused)
	}
}

func TestNativeTransientFailuresParkAndResumeWithoutReplay(t *testing.T) {
	for _, reason := range []string{"connection reset by peer", "Selected model is at capacity. Please try a different model."} {
		t.Run(reason, func(t *testing.T) {
			failure, _ := json.Marshal(map[string]any{"type": "turn.failed", "error": map[string]string{"message": reason}})
			f := recoveryFixture(t, string(failure))
			code, out := f.invoke("original task", "--wait")
			if code != output.ExitCodeFor(output.CodeAttentionRequired) || !strings.Contains(out, `"retryable":true`) || !strings.Contains(out, `"recovery_state":"paused_`) {
				t.Fatalf("park exit=%d output=%s", code, out)
			}
			id := failureExecutionIDFromOutput(t, out)
			code, replay := f.invoke("original task", "--wait")
			if code != output.ExitCodeFor(output.CodeAttentionRequired) || f.launches(t) != 1 {
				t.Fatalf("replay launched: %s", replay)
			}
			code, plan := f.command(t, "reconcile prior effects and continue", "continue", id, "--request-key", "recover-1", "--prompt-stdin", "--plan")
			if code != 0 || strings.Contains(plan, "thread-1") || !strings.Contains(plan, "<native-session>") {
				t.Fatalf("plan exit=%d: %s", code, plan)
			}
			code, resumed := f.command(t, "reconcile prior effects and continue", "continue", id, "--request-key", "recover-1", "--prompt-stdin", "--wait")
			if code != 0 || !strings.Contains(resumed, "resumed answer") || f.launches(t) != 2 {
				t.Fatalf("resume exit=%d: %s", code, resumed)
			}
			_, status := f.command(t, "", "status", id)
			if !strings.Contains(status, `"state":"cancelled"`) || !strings.Contains(status, "superseded_by") {
				t.Fatalf("unsettled source: %s", status)
			}
		})
	}
}

func TestFailedNativeSessionCanBeExplicitlyContinued(t *testing.T) {
	f := recoveryFixture(t, `{"type":"turn.failed","error":{"message":"unclassified failure"}}`)
	code, out := f.invoke("task", "--wait")
	if code != output.ExitCodeFor(output.CodeExecutionFailed) {
		t.Fatalf("exit=%d: %s", code, out)
	}
	id := failureExecutionIDFromOutput(t, out)
	code, resumed := f.command(t, "inspect prior effects and continue", "continue", id, "--request-key", "recover-failed", "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(resumed, "resumed answer") {
		t.Fatalf("resume exit=%d: %s", code, resumed)
	}
	// Carry-forward cannot manufacture an answer from a failed turn.
	code, refused := f.command(t, "next", "continue", id, "--mode", "carry_forward", "--request-key", "carry-failed", "--prompt-stdin", "--plan")
	if code == 0 || !strings.Contains(refused, "continue_source_not_completed") {
		t.Fatalf("carry-forward: %s", refused)
	}
}

func TestTransportClassificationPreservesAuthAndQuota(t *testing.T) {
	for _, message := range []string{"connect ETIMEDOUT", "getaddrinfo ENOTFOUND", "ECONNRESET", "error sending request for url"} {
		code, kind, _, retryable := classifyOutcomeFailure(message, "", "", "zcode", "")
		if code != "network" || kind != "network" || !retryable {
			t.Fatalf("%s: %s/%s/%v", message, code, kind, retryable)
		}
	}
	for _, message := range []string{"authentication required: ECONNRESET", "quota exhausted", "native execution failed"} {
		_, kind, _, _ := classifyOutcomeFailure(message, "", "", "zcode", "")
		if kind == "network" || kind == "capacity" {
			t.Fatalf("misclassified %s", message)
		}
	}
}

// The fake native store belongs to the CLI. Tests can publish a final answer
// after the client exits without a real service, model call, or background job.
func opencodeRecoveryFixture(t *testing.T, failure string, session bool) delegateFixture {
	t.Helper()
	f := carryFixture(t, "opencode")
	t.Setenv("OPENCODE_TEST_HISTORY", filepath.Join(f.root, "native-final"))
	t.Setenv("OPENCODE_TEST_EFFECTS", filepath.Join(f.root, "effects"))
	start := `printf '%s\n' '{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}'`
	if !session {
		start = ":"
	}
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'opencode 2.2.6'; exit 0; fi\n" + `if [ "$1" = session ]; then
 [ "$#" = 5 ] && [ "$2" = export ] && [ "$3" = --sanitize ] && [ "$4" = -- ] && [ "$5" = s1 ] || exit 21
 echo check >> "` + filepath.Join(f.root, "exports") + `"
 echo private-native-export >&2
 [ "${OPENCODE_TEST_EXPORT_MISSING:-0}" != 1 ] || exit 1
 [ "${OPENCODE_TEST_EXPORT_STALL:-0}" != 1 ] || exec sleep 30
 echo private-native-export
 exit 0
fi
` + "echo launch >> '" + f.count + "'\n" + `
case " $* " in
*" --session s1 -- "*)
 for arg do last="$arg"; done
 [ "$last" != original-private-task ] || exit 22
 answer='resumed answer'
 if [ -f "$OPENCODE_TEST_HISTORY" ]; then answer=$(cat "$OPENCODE_TEST_HISTORY"); fi
 printf '%s\n' '{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}' "{\"type\":\"text\",\"sessionID\":\"s1\",\"part\":{\"type\":\"text\",\"text\":\"$answer\",\"time\":{\"end\":1}}}" '{"type":"step_finish","sessionID":"s1","part":{"type":"step-finish","reason":"stop"}}';;
*" --session "*) exit 23;;
*)
 echo effect >> "$OPENCODE_TEST_EFFECTS"
 ` + start + "\n"
	if failure == "" {
		script += `printf '%s\n' '{"type":"text","sessionID":"s1","part":{"type":"text","text":"first answer","time":{"end":1}}}' '{"type":"step_finish","sessionID":"s1","part":{"type":"step-finish","reason":"stop"}}'`
	} else {
		script += "printf '%s\\n' '" + failure + "'\nexit 1"
	}
	script += "\n;;\nesac\n"
	if err := os.WriteFile(filepath.Join(f.root, "cursor-agent"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestOpenCodeTransientFailuresParkAndCollectLateAnswer(t *testing.T) {
	for _, tc := range []struct{ name, event, state string }{
		{"network", `{"type":"error","sessionID":"s1","error":{"message":"connection reset by peer"}}`, "paused_network"},
		{"capacity", `{"type":"error","sessionID":"s1","error":{"type":"provider.rate-limit","message":"busy","status":429}}`, "paused_capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := opencodeRecoveryFixture(t, tc.event, true)
			code, out := f.invoke("original-private-task", "--wait")
			if code != 10 || !strings.Contains(out, tc.state) || !strings.Contains(out, `"retryable":true`) {
				t.Fatalf("park %d: %s", code, out)
			}
			id := failureExecutionIDFromOutput(t, out)
			code, replay := f.invoke("original-private-task", "--wait")
			if code != 10 || f.launches(t) != 1 {
				t.Fatalf("replay %d: %s", code, replay)
			}
			_, events := f.command(t, "", "events", id)
			if strings.Contains(events, "original-private-task") || strings.Contains(events, `"sessionID"`) || strings.Contains(events, `"session_id"`) {
				t.Fatalf("private event data: %s", events)
			}
			code, plan := f.command(t, "inspect history; return previous final answer if complete; otherwise reconcile effects", "continue", id, "--request-key", "recover", "--prompt-stdin", "--plan")
			if code != 0 || !strings.Contains(plan, `"--session","<native-session>","--"`) || strings.Contains(plan, `"s1"`) || f.launches(t) != 1 {
				t.Fatalf("plan %d: %s", code, plan)
			}
			// Native completion happens after agentctl parked the exited client.
			if err := os.WriteFile(filepath.Join(f.root, "native-final"), []byte("late final answer"), 0600); err != nil {
				t.Fatal(err)
			}
			code, resumed := f.command(t, "inspect history; return previous final answer if complete; otherwise reconcile effects", "continue", id, "--request-key", "recover", "--prompt-stdin", "--wait")
			if code != 0 || !strings.Contains(resumed, "late final answer") || f.launches(t) != 2 {
				t.Fatalf("recovery %d: %s", code, resumed)
			}
			effects, err := os.ReadFile(filepath.Join(f.root, "effects"))
			if err != nil || string(effects) != "effect\n" {
				t.Fatalf("repeated tool effect: %q %v", effects, err)
			}
			_, status := f.command(t, "", "status", id)
			if !strings.Contains(status, `"state":"cancelled"`) || !strings.Contains(status, "superseded_by") {
				t.Fatalf("unsettled source: %s", status)
			}
		})
	}
}

func TestOpenCodeUnknownFailureCanContinueButMissingCheckpointCannot(t *testing.T) {
	for _, session := range []bool{true, false} {
		f := opencodeRecoveryFixture(t, `{"type":"error","error":{"message":"unknown native error"}}`, session)
		code, out := f.invoke("original-private-task", "--wait")
		if code != 11 {
			t.Fatalf("failure %d: %s", code, out)
		}
		id := failureExecutionIDFromOutput(t, out)
		code, out = f.command(t, "reconcile effects", "continue", id, "--request-key", "recover", "--prompt-stdin", "--wait")
		if session {
			if code != 0 || !strings.Contains(out, "resumed answer") {
				t.Fatalf("recovery %d: %s", code, out)
			}
		} else if code != 5 || !strings.Contains(out, "continue_session_unrecorded") || f.launches(t) != 1 {
			t.Fatalf("missing checkpoint %d: %s", code, out)
		}
	}
}

func TestOpenCodeDoesNotParkWithoutCheckpointOrForAuthAndQuota(t *testing.T) {
	for _, tc := range []struct {
		message string
		session bool
	}{
		{"connection reset by peer", false},
		{"authentication required: ECONNRESET", true},
		{"quota exhausted", true},
	} {
		event, err := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"message": tc.message}})
		if err != nil {
			t.Fatal(err)
		}
		f := opencodeRecoveryFixture(t, string(event), tc.session)
		code, out := f.invoke("original-private-task", "--wait")
		if code != 11 || strings.Contains(out, "paused_network") || strings.Contains(out, "paused_capacity") {
			t.Fatalf("unexpected park %d: %s", code, out)
		}
		if !tc.session {
			id := failureExecutionIDFromOutput(t, out)
			code, out = f.command(t, "recover", "continue", id, "--request-key", "missing-checkpoint", "--prompt-stdin", "--plan")
			if code != 5 || !strings.Contains(out, "continue_session_unrecorded") || f.launches(t) != 1 {
				t.Fatalf("unknown session resumed %d: %s", code, out)
			}
		}
	}
}
