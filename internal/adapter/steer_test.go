package adapter

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNegotiateSteerIsInvocationScoped(t *testing.T) {
	claudeLive := []string{"claude", "--print", "--verbose", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages"}
	for _, test := range []struct {
		name       string
		manifest   Manifest
		argv       []string
		delivery   string
		promptArgs int
		status     CapabilityStatus
		route      string
		reason     string
	}{
		{name: "claude live", manifest: claudeManifest(), argv: claudeLive, delivery: PromptDeliveryStream, status: CapabilitySupported, route: "live_input"},
		{name: "claude stream without replay", manifest: claudeManifest(), argv: claudeLive[:len(claudeLive)-1], delivery: PromptDeliveryStream, status: CapabilityUnavailable, reason: "live input argv"},
		{name: "claude one-shot", manifest: claudeManifest(), argv: []string{"claude", "--print", "--output-format", "stream-json", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityDegraded, route: "interrupt_resume"},
		{name: "claude without persistence", manifest: claudeManifest(), argv: []string{"claude", "--print", "--no-session-persistence", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "disables a session"},
		{name: "claude embedded prompt", manifest: claudeManifest(), argv: []string{"claude", "--print", "task"}, status: CapabilityUnavailable, reason: "agentctl-managed prompt delivery"},
		{name: "codex stdin", manifest: codexManifest(), argv: []string{"codex", "exec", "--json", "--sandbox", "read-only", "-"}, delivery: PromptDeliveryStdin, status: CapabilityDegraded, route: "interrupt_resume"},
		{name: "codex ephemeral", manifest: codexManifest(), argv: []string{"codex", "exec", "--json", "--ephemeral", "-"}, delivery: PromptDeliveryStdin, status: CapabilityUnavailable, reason: "not accepted"},
		{name: "cursor", manifest: cursorManifest(), argv: []string{"cursor-agent", "--print", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "does not persist the prompt"},
		{name: "omp", manifest: ompManifest(), argv: []string{"omp", "-p", "--mode", "json", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "interrupted during its first turn"},
		{name: "devin", manifest: devinManifest(), argv: []string{"devin", "-p", "--", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "session id"},
		{name: "zcode", manifest: zcodeManifest(), argv: []string{"zcode", "--json", "--prompt", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "no verified steering"},
		{name: "generic", manifest: genericManifest(), argv: []string{"/bin/echo", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "no verified steering route"},
		{name: "multica", manifest: multicaManifest(), argv: []string{"multica"}, delivery: PromptDeliveryStdin, status: CapabilityUnavailable, reason: "Multica owns run input"},
		{name: "static claude", manifest: claudeManifest(), status: CapabilityDegraded},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := NegotiateSteer(test.manifest, test.argv, test.delivery, test.promptArgs)
			if got.Name != CapabilitySteer || got.Status != test.status {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Reason, test.status)
			}
			if route, _ := got.Constraints["delivery"].(string); route != test.route {
				t.Fatalf("delivery = %q, want %q", route, test.route)
			}
			if !strings.Contains(got.Reason, test.reason) {
				t.Fatalf("reason = %q, want it to mention %q", got.Reason, test.reason)
			}
		})
	}
}

func TestEveryManifestDeclaresSteerAndResume(t *testing.T) {
	for _, manifest := range []Manifest{genericManifest(), codexManifest(), cursorManifest(), claudeManifest(), ompManifest(), zcodeManifest(), devinManifest(), multicaManifest()} {
		count := 0
		for _, declaration := range manifest.Capabilities {
			if declaration.Name == CapabilitySteer || declaration.Name == CapabilityResume {
				count++
			}
		}
		if count != 2 {
			t.Fatalf("%s declares steer and resume %d times, want once each", manifest.Adapter, count)
		}
	}
}

func TestCodexResumeArgv(t *testing.T) {
	got, err := codexResumeArgv([]string{"codex", "exec", "--json", "--model", "m", "-c", `service_tier="default"`, "--sandbox", "read-only", "-"}, "thread-1", PromptDeliveryStdin)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "exec", "resume", "--json", "--model", "m", "-c", `service_tier="default"`, "-c", `sandbox_mode="read-only"`, "thread-1", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stdin resume argv = %q", got)
	}
	got, err = codexResumeArgv([]string{"codex", "exec", "--json", "--model=m", "--"}, "thread-1", PromptDeliveryArgv)
	if err != nil || !reflect.DeepEqual(got, []string{"codex", "exec", "resume", "--json", "--model=m", "thread-1"}) {
		t.Fatalf("argv resume argv = %q err=%v", got, err)
	}
	for _, base := range [][]string{{"codex", "exec", "--json", "--cd", "/tmp", "-"}, {"codex", "review", "-"}, {"codex", "exec", "fork", "x", "-"}, {"codex", "exec", "resume", "x", "stray", "-"}} {
		if _, err := codexResumeArgv(base, "thread-1", PromptDeliveryStdin); err == nil {
			t.Fatalf("argv %q should not be resumable", base)
		}
	}
}

func TestClaudeResumeArgvKeepsPromptLast(t *testing.T) {
	got, err := claudeResumeArgv([]string{"claude", "--print", "--model", "m", "--"}, "session-1", PromptDeliveryArgv)
	if err != nil || !reflect.DeepEqual(got, []string{"claude", "--print", "--model", "m", "--resume", "session-1"}) {
		t.Fatalf("resume argv = %q err=%v", got, err)
	}
	if _, err := claudeResumeArgv([]string{"claude", "--print", "--continue"}, "session-1", PromptDeliveryArgv); err == nil {
		t.Fatal("an argv that continues an ambient session must not be resumable")
	}
}

// boundedContext turns a native fixture that never finishes into a prompt,
// named test failure instead of a package-wide timeout.
func boundedContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitForEvent(t *testing.T, a Adapter, ref SourceRef, sourceState string) []Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := a.Events(context.Background(), EventsRequest{Ref: ref})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.SourceState == sourceState {
				return events
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s event observed", sourceState)
	return nil
}

// fakeLiveAgent speaks the acknowledgement half of Claude Code's streaming
// input protocol. It holds its first turn open until a second message arrives,
// so the first result is emitted while that message is still unacknowledged.
const fakeLiveAgent = `
IFS= read -r first
printf '%s\n' '{"type":"system","subtype":"init","session_id":"live-1"}'
printf '%s\n' '{"type":"user","isReplay":true,"session_id":"live-1"}'
IFS= read -r second
printf '%s\n' '{"type":"result","result":"turn-1","is_error":false,"session_id":"live-1"}'
printf '%s\n' '{"type":"user","isReplay":true,"session_id":"live-1"}'
printf '%s\n' '{"type":"result","result":"turn-2","is_error":false,"session_id":"live-1"}'
while IFS= read -r extra; do :; done
`

func TestLiveInputSteerExtendsTheSessionByOneTurn(t *testing.T) {
	path := fixtureExecutable(t, fakeLiveAgent)
	a := NewClaudeCode()
	argv := []string{path, "--print", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages"}
	launched, err := a.Launch(context.Background(), LaunchRequest{Argv: argv, Stdin: []byte("task"), PromptDelivery: PromptDeliveryStream, StartOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ref := launched.Session.Ref
	waitForEvent(t, a, ref, "input_acknowledged")
	steered, err := a.Steer(context.Background(), SteerRequest{Ref: ref, Message: []byte("change course")})
	if err != nil {
		t.Fatal(err)
	}
	if steered.Delivery != SteerLiveInput {
		t.Fatalf("delivery = %s", steered.Delivery)
	}
	result, err := a.Wait(boundedContext(t), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Content != "turn-2" {
		t.Fatalf("final result = %#v", result)
	}
	events, _ := a.Events(context.Background(), EventsRequest{Ref: ref})
	states := []string{}
	for _, event := range events {
		states = append(states, event.SourceState)
	}
	joined := strings.Join(states, ",")
	if !strings.Contains(joined, "turn_completed") || strings.Count(joined, "input_acknowledged") != 2 {
		t.Fatalf("events = %s", joined)
	}
	_, err = a.Steer(context.Background(), SteerRequest{Ref: ref, Message: []byte("too late")})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrInvalidState {
		t.Fatalf("steer after the final result = %v", err)
	}
}

type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }

func TestLiveInputLateSteerKeepsCompletedTurn(t *testing.T) {
	record := &processRecord{live: &liveSession{stdin: discardCloser{}, pending: 1}, done: make(chan struct{}), maxOutput: defaultOutputLimit, ref: SourceRef{Adapter: "claude-code", OpaqueID: "live-1"}}
	record.ingestObservation(parsedObservation{Kind: "terminal", Terminal: true, Success: true, State: StateCompleted, Content: "only-turn", SourceState: "result"})
	if record.result != nil {
		t.Fatalf("a pending live message must demote the terminal record: %#v", record.result)
	}
	record.finish(nil)
	if record.result == nil || !record.result.Success || record.result.State != StateCompleted || record.result.Content != "only-turn" {
		t.Fatalf("late steer orphaned the completed turn: %#v", record.result)
	}
}

func TestLiveInputSteerRacingExitKeepsResult(t *testing.T) {
	path := fixtureExecutable(t, `
IFS= read -r first
printf '%s\n' '{"type":"system","subtype":"init","session_id":"live-1"}'
printf '%s\n' '{"type":"user","isReplay":true,"session_id":"live-1"}'
printf '%s\n' '{"type":"result","result":"only-turn","is_error":false,"session_id":"live-1"}'
`)
	a := NewClaudeCode()
	argv := []string{path, "--print", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages"}
	launched, err := a.Launch(context.Background(), LaunchRequest{Argv: argv, Stdin: []byte("task"), PromptDelivery: PromptDeliveryStream, StartOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ref := launched.Session.Ref
	waitForEvent(t, a, ref, "input_acknowledged")
	_, _ = a.Steer(context.Background(), SteerRequest{Ref: ref, Message: []byte("too late")})
	result, err := a.Wait(boundedContext(t), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.State != StateCompleted || result.Content != "only-turn" {
		t.Fatalf("racy steer discarded the completed answer: %#v", result)
	}
}

func TestStreamDeliveryRequiresLiveArgv(t *testing.T) {
	path := fixtureExecutable(t, "exit 0")
	_, err := NewClaudeCode().Launch(context.Background(), LaunchRequest{Argv: []string{path, "--print"}, Stdin: []byte("task"), PromptDelivery: PromptDeliveryStream})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrCapabilityUnavailable {
		t.Fatalf("launch error = %v", err)
	}
}

// fakeResumableAgent is a one-shot CLI with a `resume` form. The first
// invocation never finishes on its own; the resumed one answers with the
// message it was given.
const fakeResumableAgent = `
case " $* " in
*" resume "*)
  IFS= read -r message
  args=$(printf '%s' "$*" | tr '"' "'")
  printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}'
  printf '%s\n' '{"type":"steer.fixture.resumed"}'
  if [ "$message" = redirect ]; then sleep 60; fi
  printf '%s\n' "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"resumed with $message via $args\"}}"
  printf '%s\n' '{"type":"turn.completed"}'
  ;;
*)
  sleep "${STEER_FIXTURE_DELAY:-0}"
  printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}'
  sleep 60
  ;;
esac
`

func TestInterruptResumeSteerContinuesTheSameSession(t *testing.T) {
	path := fixtureExecutable(t, fakeResumableAgent)
	a := NewCodex()
	launched, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path, "exec", "--json", "--sandbox", "read-only", "-"}, Stdin: []byte("task\n"), PromptDelivery: PromptDeliveryStdin, StartOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ref := launched.Session.Ref
	before := waitForEvent(t, a, ref, "thread.started")

	_, err = a.Steer(context.Background(), SteerRequest{Ref: ref, Message: []byte("redirect\n")})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrCapabilityUnavailable {
		t.Fatalf("steer without interrupt permission = %v", err)
	}
	if result, _ := a.Result(context.Background(), ResultRequest{Ref: ref}); result.State != StateRunning {
		t.Fatalf("a refused steer must not disturb the session: %#v", result)
	}

	steered, err := a.Steer(context.Background(), SteerRequest{Ref: ref, Message: []byte("redirect\n"), AllowInterrupt: true})
	if err != nil {
		t.Fatal(err)
	}
	if steered.Delivery != SteerInterruptResume {
		t.Fatalf("delivery = %s", steered.Delivery)
	}
	// A steered session can be steered again: the resumed argv already
	// selects the session and must not be rejected for that.
	waitForEvent(t, a, ref, "steer.fixture.resumed")
	if _, err := a.Steer(context.Background(), SteerRequest{Ref: ref, Message: []byte("again\n"), AllowInterrupt: true}); err != nil {
		t.Fatalf("second steer: %v", err)
	}
	// The owner keeps using the reference it was given at launch.
	result, err := a.Wait(boundedContext(t), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("final result = %#v", result)
	}
	if !strings.Contains(result.Content, "exec resume --json -c sandbox_mode='read-only' thread-1 -") {
		t.Fatalf("resume argv not as negotiated: %s", result.Content)
	}
	if !strings.Contains(result.Content, "resumed with again") {
		t.Fatalf("a second steer must reach the same session: %s", result.Content)
	}
	after, err := a.Events(context.Background(), EventsRequest{Ref: ref, Cursor: before[len(before)-1].Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) == 0 || after[0].Sequence != before[len(before)-1].Sequence+1 {
		t.Fatalf("resumed events must continue the same cursor: %#v", after)
	}
}

func TestInterruptResumeWaitsForTheNativeSessionID(t *testing.T) {
	t.Setenv("STEER_FIXTURE_DELAY", "30")
	path := fixtureExecutable(t, fakeResumableAgent)
	a := NewCodex()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	launched, err := a.Launch(ctx, LaunchRequest{Argv: []string{path, "exec", "--json", "-"}, Stdin: []byte("task\n"), PromptDelivery: PromptDeliveryStdin, StartOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Steer(ctx, SteerRequest{Ref: launched.Session.Ref, Message: []byte("redirect\n"), AllowInterrupt: true})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrInvalidState || !adapterErr.Retryable {
		t.Fatalf("steer before session discovery = %v", err)
	}
	if result, _ := a.Result(ctx, ResultRequest{Ref: launched.Session.Ref}); result.State != StateRunning {
		t.Fatalf("an early steer must not interrupt an unresumable session: %#v", result)
	}
	_ = a.Cancel(context.Background(), CancelRequest{Ref: launched.Session.Ref, Signal: "kill"})
}

func TestSteerRejectsSessionsWithoutARoute(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"system","subtype":"init","session_id":"cursor-1"}'; sleep 60`)
	a := NewCursor()
	launched, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path, "--print", "task"}, PromptDelivery: PromptDeliveryArgv, PromptArgs: 1, StartOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Steer(context.Background(), SteerRequest{Ref: launched.Session.Ref, Message: []byte("redirect"), AllowInterrupt: true})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrCapabilityUnavailable {
		t.Fatalf("steer = %v", err)
	}
	if result, _ := a.Result(context.Background(), ResultRequest{Ref: launched.Session.Ref}); result.State != StateRunning {
		t.Fatalf("a refused steer must not disturb the session: %#v", result)
	}
	_ = a.Cancel(context.Background(), CancelRequest{Ref: launched.Session.Ref, Signal: "kill"})
}

func TestNegotiateResumeIsSeparateFromSteer(t *testing.T) {
	for _, test := range []struct {
		name       string
		manifest   Manifest
		argv       []string
		delivery   string
		promptArgs int
		status     CapabilityStatus
		reason     string
	}{
		{name: "claude stream", manifest: claudeManifest(), argv: []string{"claude", "--print", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages"}, delivery: PromptDeliveryStream, status: CapabilitySupported},
		{name: "codex", manifest: codexManifest(), argv: []string{"codex", "exec", "--json", "-"}, delivery: PromptDeliveryStdin, status: CapabilitySupported},
		// Cursor and OMP keep a completed turn but not an interrupted one.
		{name: "cursor", manifest: cursorManifest(), argv: []string{"cursor-agent", "--print", "--output-format", "stream-json", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilitySupported},
		{name: "omp", manifest: ompManifest(), argv: []string{"omp", "-p", "--mode", "json", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilitySupported},
		{name: "omp without a session", manifest: ompManifest(), argv: []string{"omp", "-p", "--no-session", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "disables a session"},
		{name: "cursor embedded prompt", manifest: cursorManifest(), argv: []string{"cursor-agent", "--print", "task"}, status: CapabilityUnavailable, reason: "agentctl-managed prompt delivery"},
		{name: "devin", manifest: devinManifest(), argv: []string{"devin", "-p", "--", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "session id"},
		{name: "zcode", manifest: zcodeManifest(), argv: []string{"zcode", "--json", "--prompt", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "no verified native session resume"},
		{name: "generic", manifest: genericManifest(), argv: []string{"/bin/echo", "task"}, delivery: PromptDeliveryArgv, promptArgs: 1, status: CapabilityUnavailable, reason: "no verified native session resume"},
		{name: "multica", manifest: multicaManifest(), argv: []string{"multica"}, delivery: PromptDeliveryStdin, status: CapabilityUnavailable, reason: "Multica owns"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := NegotiateResume(test.manifest, test.argv, test.delivery, test.promptArgs)
			if got.Name != CapabilityResume || got.Status != test.status || !strings.Contains(got.Reason, test.reason) {
				t.Fatalf("resume = %s (%q), want %s mentioning %q", got.Status, got.Reason, test.status, test.reason)
			}
		})
	}
}

func TestContinuationArgv(t *testing.T) {
	for _, test := range []struct {
		adapter, delivery string
		base, want        []string
	}{
		{adapter: "cursor", delivery: PromptDeliveryArgv, base: []string{"cursor-agent", "--print", "--output-format", "stream-json", "--model", "m", "--force"}, want: []string{"cursor-agent", "--print", "--output-format", "stream-json", "--model", "m", "--force", "--resume", "s-1"}},
		{adapter: "omp", delivery: PromptDeliveryArgv, base: []string{"omp", "--no-prewalk", "-p", "--mode", "json", "--model", "m"}, want: []string{"omp", "--no-prewalk", "-p", "--mode", "json", "--model", "m", "--resume", "s-1"}},
		{adapter: "claude-code", delivery: PromptDeliveryStream, base: []string{"claude", "--print", "--input-format", "stream-json"}, want: []string{"claude", "--print", "--input-format", "stream-json", "--resume", "s-1"}},
		{adapter: "codex", delivery: PromptDeliveryStdin, base: []string{"codex", "exec", "--json", "-"}, want: []string{"codex", "exec", "resume", "--json", "s-1", "-"}},
	} {
		got, err := ContinuationArgv(test.adapter, test.base, test.delivery, "s-1")
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s: argv=%q err=%v", test.adapter, got, err)
		}
	}
	for _, adapterName := range []string{"devin", "zcode", "generic-process", "multica"} {
		_, err := ContinuationArgv(adapterName, []string{adapterName, "-p"}, PromptDeliveryArgv, "s-1")
		var adapterErr *AdapterError
		if !errors.As(err, &adapterErr) || adapterErr.Code != ErrCapabilityUnavailable {
			t.Fatalf("%s: err=%v", adapterName, err)
		}
	}
	if _, err := ContinuationArgv("cursor", []string{"cursor-agent", "--print"}, PromptDeliveryArgv, " "); err == nil {
		t.Fatal("an unknown session id must not build a resume argv")
	}
}

func TestResumeArgvAcceptsAnArgvThatAlreadyResumes(t *testing.T) {
	codex, err := codexResumeArgv([]string{"codex", "exec", "resume", "--json", "-c", `sandbox_mode="read-only"`, "old-thread", "-"}, "new-thread", PromptDeliveryStdin)
	if err != nil || !reflect.DeepEqual(codex, []string{"codex", "exec", "resume", "--json", "-c", `sandbox_mode="read-only"`, "new-thread", "-"}) {
		t.Fatalf("codex argv=%q err=%v", codex, err)
	}
	claude, err := claudeResumeArgv([]string{"claude", "--print", "--resume", "old", "--model", "m"}, "new", PromptDeliveryArgv)
	if err != nil || !reflect.DeepEqual(claude, []string{"claude", "--print", "--model", "m", "--resume", "new"}) {
		t.Fatalf("claude argv=%q err=%v", claude, err)
	}
	cursor, err := cursorResumeArgv([]string{"cursor-agent", "--print", "--resume=old"}, "new", PromptDeliveryArgv)
	if err != nil || !reflect.DeepEqual(cursor, []string{"cursor-agent", "--print", "--resume", "new"}) {
		t.Fatalf("cursor argv=%q err=%v", cursor, err)
	}
	// A follow-up turn on Codex stays steerable.
	steer := NegotiateSteer(codexManifest(), []string{"codex", "exec", "resume", "--json", "thread-1", "-"}, PromptDeliveryStdin, 0)
	if steer.Status != CapabilityDegraded {
		t.Fatalf("steer on a resumed codex argv = %s (%s)", steer.Status, steer.Reason)
	}
}
