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
	code, out = f.command(t, "park-again", "continue", first, "--request-key", "second-park", "--prompt-stdin", "--wait")
	if code != 10 {
		t.Fatalf("second park exit=%d: %s", code, out)
	}
	second := failureExecutionIDFromOutput(t, out)
	code, refused := f.command(t, "recover", "continue", first, "--request-key", "stale-source", "--prompt-stdin", "--plan")
	if code != 8 || !strings.Contains(refused, "continue_not_latest") || !strings.Contains(refused, second) {
		t.Fatalf("stale: %s", refused)
	}
	code, out = f.command(t, "recover", "continue", second, "--request-key", "final-recovery", "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(out, "recovered answer") || f.launches(t) != 3 {
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
