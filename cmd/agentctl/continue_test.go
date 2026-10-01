package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// continueAgent reports one native session id and says whether the invocation
// resumed that exact session. A resumed turn takes long enough for two
// invocations to overlap.
const continueAgent = `case " $* " in
*" --resume chat-1 "*) kind=resumed; sleep "${CONTINUE_FIXTURE_DELAY:-0}";;
*" --resume "*) kind=wrong-session;;
*) kind=first;;
esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"chat-1"}' "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"$kind answer\"}"`

func (f delegateFixture) command(t *testing.T, stdin string, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if stdin != "" {
		a.stdin = strings.NewReader(stdin)
		a.stdinIsTerminal = func() bool { return false }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code := a.run(ctx, append([]string{"--config", f.config, "--journal", f.journal}, args...))
	return code, stdout.String()
}

func resultID(t *testing.T, out string) string {
	t.Helper()
	var doc struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Result.ID == "" {
		t.Fatalf("no execution id in %s", out)
	}
	return doc.Result.ID
}

func (f delegateFixture) launches(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.count)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "launch")
}

func TestContinueRunsFollowUpTurnsOnTheSameNativeSession(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	code, out := f.invoke("first task", "--wait")
	if code != 0 || !strings.Contains(out, "first answer") {
		t.Fatalf("delegate exit=%d output=%s", code, out)
	}
	first := resultID(t, out)
	if _, status := f.command(t, "", "status", first); strings.Contains(status, "chat-1") || !strings.Contains(status, `"kind":"native_session"`) {
		t.Fatalf("status must bind the native session without exposing its id: %s", status)
	}

	code, plan := f.command(t, "review feedback", "continue", first, "--request-key", "turn-2", "--prompt-stdin", "--plan")
	if code != 0 || !strings.Contains(plan, `"--resume","<native-session>"`) || strings.Contains(plan, "chat-1") || strings.Contains(plan, "review feedback") {
		t.Fatalf("plan exit=%d output=%s", code, plan)
	}
	assertResultMatchesSchemaShape(t, plan, "continue-result.schema.json")
	if f.launches(t) != 1 {
		t.Fatalf("a plan must not launch: %d", f.launches(t))
	}

	code, out = f.command(t, "review feedback", "continue", first, "--request-key", "turn-2", "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(out, "resumed answer") || !strings.Contains(out, `"continues":"`+first+`"`) || !strings.Contains(out, `"reused":false`) {
		t.Fatalf("continue exit=%d output=%s", code, out)
	}
	assertResultMatchesSchemaShape(t, out, "continue-result.schema.json")
	second := resultID(t, out)
	if second == first {
		t.Fatal("a follow-up turn must be a new execution")
	}
	// The third turn is built from the original recipe, not from an argv
	// that already resumes.
	code, out = f.command(t, "more feedback", "continue", second, "--request-key", "turn-3", "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(out, "resumed answer") {
		t.Fatalf("third turn exit=%d output=%s", code, out)
	}
	third := resultID(t, out)

	code, replay := f.command(t, "review feedback", "continue", first, "--request-key", "turn-2", "--prompt-stdin", "--wait")
	if code != 0 || resultID(t, replay) != second || !strings.Contains(replay, `"reused":true`) {
		t.Fatalf("replay exit=%d output=%s", code, replay)
	}
	code, changed := f.command(t, "different feedback", "continue", first, "--request-key", "turn-2", "--prompt-stdin", "--wait")
	if code == 0 || !strings.Contains(changed, `"code":"conflict"`) {
		t.Fatalf("a reused key with a different prompt must conflict: exit=%d output=%s", code, changed)
	}
	code, stale := f.command(t, "late feedback", "continue", first, "--request-key", "turn-late", "--prompt-stdin", "--wait")
	if code == 0 || !strings.Contains(stale, "continue_not_latest") || !strings.Contains(stale, `"latest_execution_id":"`+third+`"`) {
		t.Fatalf("stale source exit=%d output=%s", code, stale)
	}
	if f.launches(t) != 3 {
		t.Fatalf("launches=%d, want one per turn", f.launches(t))
	}
	_, status := f.command(t, "", "status", first)
	if !strings.Contains(status, `"superseded_by":"`+second+`"`) {
		t.Fatalf("the continued turn must point at its continuation: %s", status)
	}
	if _, events := f.command(t, "", "events", first); !strings.Contains(events, `"kind":"superseded"`) {
		t.Fatalf("missing superseded event: %s", events)
	}
}

func TestContinueAdmitsOnlyOneConcurrentTurn(t *testing.T) {
	t.Setenv("CONTINUE_FIXTURE_DELAY", "1")
	f := newDelegateFixture(t, continueAgent)
	code, out := f.invoke("first task", "--wait")
	if code != 0 {
		t.Fatalf("delegate exit=%d output=%s", code, out)
	}
	first := resultID(t, out)
	outputs := make([]string, 2)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range outputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], outputs[i] = f.command(t, "feedback", "continue", first, "--request-key", []string{"racer-a", "racer-b"}[i], "--prompt-stdin", "--wait")
		}(i)
	}
	wg.Wait()
	succeeded, refused := 0, 0
	for i := range outputs {
		switch {
		case codes[i] == 0 && strings.Contains(outputs[i], "resumed answer"):
			succeeded++
		case strings.Contains(outputs[i], "continue_in_progress"):
			refused++
		default:
			t.Fatalf("unexpected outcome exit=%d output=%s", codes[i], outputs[i])
		}
	}
	if succeeded != 1 || refused != 1 || f.launches(t) != 2 {
		t.Fatalf("succeeded=%d refused=%d launches=%d", succeeded, refused, f.launches(t))
	}
}

func TestContinueRefusedKeyCanRetryFromLatestTurn(t *testing.T) {
	t.Setenv("CONTINUE_FIXTURE_DELAY", "1")
	f := newDelegateFixture(t, continueAgent)
	code, out := f.invoke("first task", "--wait")
	if code != 0 {
		t.Fatalf("delegate exit=%d output=%s", code, out)
	}
	first := resultID(t, out)
	keys := []string{"racer-a", "racer-b"}
	outputs := make([]string, 2)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range outputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], outputs[i] = f.command(t, "feedback", "continue", first, "--request-key", keys[i], "--prompt-stdin", "--wait")
		}(i)
	}
	wg.Wait()
	winnerID, refusedKey := "", ""
	for i := range outputs {
		switch {
		case codes[i] == 0 && strings.Contains(outputs[i], "resumed answer"):
			winnerID = resultID(t, outputs[i])
		case strings.Contains(outputs[i], "continue_in_progress"):
			refusedKey = keys[i]
		default:
			t.Fatalf("unexpected outcome exit=%d output=%s", codes[i], outputs[i])
		}
	}
	if winnerID == "" || refusedKey == "" {
		t.Fatalf("winner=%q refusedKey=%q outputs=%q", winnerID, refusedKey, outputs)
	}
	code, out = f.command(t, "feedback", "continue", winnerID, "--request-key", refusedKey, "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(out, "resumed answer") || resultID(t, out) == winnerID {
		t.Fatalf("retry refused key exit=%d output=%s", code, out)
	}
}

// continueFlakyAgent fails its first resumed turn and answers the next one.
const continueFlakyAgent = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"chat-1"}'
case " $* " in
*" --resume chat-1 "*)
  sleep 1
  if rm "$CONTINUE_FIXTURE_FAIL_ONCE" 2>/dev/null; then
    printf '%s\n' '{"type":"result","subtype":"error","is_error":true,"result":"boom"}'
    exit 0
  fi
  kind=resumed;;
*) kind=first;;
esac
printf '%s\n' "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"$kind answer\"}"`

// A follow-up that loses the admission race never launched, so its request
// key is still owed its turn: retrying it must not recover the refusal.
func TestContinueRefusedAdmissionDoesNotUseUpTheRequestKey(t *testing.T) {
	f := newDelegateFixture(t, continueFlakyAgent)
	failOnce := filepath.Join(f.root, "fail-once")
	t.Setenv("CONTINUE_FIXTURE_FAIL_ONCE", failOnce)
	code, out := f.invoke("first task", "--wait")
	if code != 0 {
		t.Fatalf("delegate exit=%d output=%s", code, out)
	}
	first := resultID(t, out)
	if err := os.WriteFile(failOnce, nil, 0600); err != nil {
		t.Fatal(err)
	}
	keys := []string{"racer-a", "racer-b"}
	outputs := make([]string, 2)
	var wg sync.WaitGroup
	for i := range outputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, outputs[i] = f.command(t, "feedback", "continue", first, "--request-key", keys[i], "--prompt-stdin", "--wait")
		}(i)
	}
	wg.Wait()
	refused := -1
	for i := range outputs {
		if strings.Contains(outputs[i], "continue_in_progress") {
			refused = i
		} else if !strings.Contains(outputs[i], "execution_failed") {
			t.Fatalf("unexpected outcome: %s", outputs[i])
		}
	}
	if refused < 0 || f.launches(t) != 2 {
		t.Fatalf("launches=%d outputs=%v", f.launches(t), outputs)
	}
	// The admitted turn failed, so the session is free again and the refused
	// key gets the turn it asked for.
	code, out = f.command(t, "feedback", "continue", first, "--request-key", keys[refused], "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(out, "resumed answer") || f.launches(t) != 3 {
		t.Fatalf("retry of the refused key exit=%d launches=%d output=%s", code, f.launches(t), out)
	}
	// And that turn is what the key recovers from now on.
	code, again := f.command(t, "feedback", "continue", first, "--request-key", keys[refused], "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(again, `"reused":true`) || resultID(t, again) != resultID(t, out) || f.launches(t) != 3 {
		t.Fatalf("replay exit=%d launches=%d output=%s", code, f.launches(t), again)
	}
}

func TestContinueRejectsSourcesItCannotContinue(t *testing.T) {
	failed := newDelegateFixture(t, `printf '%s\n' '{"type":"system","subtype":"init","session_id":"chat-1"}' '{"type":"result","subtype":"error","is_error":true,"result":"boom"}'`)
	_, out := failed.invoke("task", "--wait")
	var doc struct {
		Error struct {
			Details struct {
				ExecutionID string `json:"execution_id"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Error.Details.ExecutionID == "" {
		t.Fatalf("failed delegate output=%s", out)
	}
	code, refused := failed.command(t, "feedback", "continue", doc.Error.Details.ExecutionID, "--request-key", "k", "--prompt-stdin")
	if code == 0 || !strings.Contains(refused, "continue_source_not_completed") || failed.launches(t) != 1 {
		t.Fatalf("failed source exit=%d output=%s", code, refused)
	}

	// Expert run records no launch recipe to rebuild a resume invocation from.
	f := newDelegateFixture(t, continueAgent)
	code, out = f.command(t, "task", "run", "--adapter", "cursor", "--prompt-stdin", "--prompt-delivery", "argv", "--", filepath.Join(f.root, "cursor-agent"), "--print", "--output-format", "stream-json")
	if code != 0 || !strings.Contains(out, `"state":"completed"`) {
		t.Fatalf("run exit=%d output=%s", code, out)
	}
	code, refused = f.command(t, "feedback", "continue", resultID(t, out), "--request-key", "k", "--prompt-stdin")
	if code == 0 || !strings.Contains(refused, "continue_launch_plan_missing") || f.launches(t) != 1 {
		t.Fatalf("run-launched source exit=%d output=%s", code, refused)
	}
}

func TestContinueFlagsFailClosed(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"exec-a", "--prompt-stdin"},
		{"exec-a", "--request-key", "k"},
		{"exec-a", "--request-key", "k", "--prompt-stdin", "--prompt-file", "a.md"},
		{"exec-a", "exec-b", "--request-key", "k", "--prompt-stdin"},
		{"exec-a", "--request-key", "k", "--prompt-stdin", "--content"},
		{"exec-a", "--request-key", "k", "--prompt-stdin", "--plan", "--wait"},
		{"exec-a", "--request-key", "k", "--prompt-stdin", "--timeout", "-1s"},
		{"exec-a", "--request-key", "k", "--prompt-stdin", "--cwd", "/tmp"},
	} {
		if _, _, _, problem := parseContinue(args); problem == nil {
			t.Fatalf("args %v must be rejected", args)
		}
	}
}
