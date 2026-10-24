package anxrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeRunner records anx invocations and replies with canned envelopes.
type fakeRunner struct {
	argvs     [][]string
	stdins    []string
	envelopes []string
	err       error
}

func (f *fakeRunner) run(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	f.argvs = append(f.argvs, append([]string(nil), argv...))
	f.stdins = append(f.stdins, string(stdin))
	if len(f.envelopes) > 0 {
		next := f.envelopes[0]
		f.envelopes = f.envelopes[1:]
		return []byte(next), nil
	}
	return []byte(`{"ok":true,"result":{"session":{"id":"sess-1"}}}`), nil
}

func TestCLISenderArgvAndBodies(t *testing.T) {
	runner := &fakeRunner{}
	sender := &CLISender{options: CLIOptions{WorkspaceAlias: "anx", Timeout: 5 * time.Second}, run: runner.run}
	registration := testRegistration("exec-a", PhaseTerminal, 4)
	registration.CardRefs = []string{"card:watch-redesign", "card:other"}
	if err := sender.Send(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	if len(runner.argvs) != 3 {
		t.Fatalf("invocations = %d (session + 2 cards)", len(runner.argvs))
	}
	sessionArgv := runner.argvs[0]
	joined := strings.Join(sessionArgv, " ")
	for _, want := range []string{"--json", "--workspace anx", "api sessions.register", "--json-body -"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("session argv missing %q: %v", want, sessionArgv)
		}
	}
	if strings.Contains(joined, "--base-url") {
		t.Fatalf("base URL passed without configuration: %v", sessionArgv)
	}
	var sessionBody map[string]any
	if err := json.Unmarshal([]byte(runner.stdins[0]), &sessionBody); err != nil {
		t.Fatal(err)
	}
	if sessionBody["native_session_id_kind"] != NativeSessionIDKind || sessionBody["provider"] != "claude" || sessionBody["activity"] != "idle" || sessionBody["sequence"] != float64(4) || sessionBody["host_scope"] != registration.OriginHostID {
		t.Fatalf("session body = %#v", sessionBody)
	}
	capabilities, _ := sessionBody["capabilities"].(map[string]any)
	if capabilities == nil || capabilities["resume"] != "unknown" {
		t.Fatalf("capabilities = %#v", sessionBody["capabilities"])
	}
	cardArgv := strings.Join(runner.argvs[1], " ")
	if !strings.Contains(cardArgv, "api work.participants.register") || !strings.Contains(cardArgv, "--param card_ref=card:watch-redesign") {
		t.Fatalf("participant argv wrong: %v", runner.argvs[1])
	}
	var participantBody map[string]any
	if err := json.Unmarshal([]byte(runner.stdins[1]), &participantBody); err != nil {
		t.Fatal(err)
	}
	// The session id from the register response feeds participation.
	if participantBody["session_id"] != "sess-1" {
		t.Fatalf("participant session id = %#v", participantBody["session_id"])
	}
	if participantBody["activity"] != "idle" {
		t.Fatalf("participant activity = %#v", participantBody["activity"])
	}
}

func TestCLISenderPassesExplicitEndpointOptions(t *testing.T) {
	runner := &fakeRunner{}
	sender := &CLISender{options: CLIOptions{WorkspaceAlias: "anx", BaseURL: "https://anx.example.test", ConfigDir: "/opt/anx-config", Timeout: time.Second}, run: runner.run}
	if err := sender.Send(context.Background(), testRegistration("exec-a", PhaseStart, 1)); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.argvs[0], " ")
	if !strings.Contains(joined, "--base-url https://anx.example.test") || !strings.Contains(joined, "--config-dir /opt/anx-config") {
		t.Fatalf("explicit options not passed: %v", runner.argvs[0])
	}
}

func TestCLISenderStartRegistersActive(t *testing.T) {
	runner := &fakeRunner{}
	sender := &CLISender{options: CLIOptions{}, run: runner.run}
	if err := sender.Send(context.Background(), testRegistration("exec-a", PhaseStart, 1)); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(runner.stdins[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["activity"] != "active" {
		t.Fatalf("start activity = %#v", body["activity"])
	}
}

func TestCLISenderRequiresNativeSession(t *testing.T) {
	sender := &CLISender{options: CLIOptions{}, run: func(context.Context, []string, []byte) ([]byte, error) {
		t.Fatal("no invocation expected")
		return nil, nil
	}}
	registration := testRegistration("exec-a", PhaseTerminal, 1)
	registration.NativeSession = nil
	err := sender.Send(context.Background(), registration)
	var sendErr *SendError
	if err == nil || !errorsAs(err, &sendErr) || !sendErr.Fatal || sendErr.Reason != ReasonNoSession {
		t.Fatalf("expected fatal no-session error, got %v", err)
	}
}

func errorsAs(err error, target **SendError) bool {
	sendErr, ok := err.(*SendError)
	if ok {
		*target = sendErr
	}
	return ok
}

func TestCLISenderClassifiesANXErrors(t *testing.T) {
	for code, wantReason := range map[string]string{
		"auth_required":        ReasonAuthRequired,
		"invalid_token":        ReasonAuthRequired,
		"forbidden":            ReasonAuthRequired,
		"not_found":            ReasonCardUnresolved,
		"conflict":             ReasonConflict,
		"invalid_request":      ReasonRejected,
		"sessions_unavailable": "",
	} {
		runner := &fakeRunner{}
		runner.envelopes = []string{`{"ok":false,"error":{"code":"` + code + `","message":"nope"}}`}
		sender := &CLISender{options: CLIOptions{}, run: runner.run}
		err := sender.Send(context.Background(), testRegistration("exec-a", PhaseTerminal, 1))
		var sendErr *SendError
		if err == nil || !errorsAs(err, &sendErr) {
			t.Fatalf("%s: expected SendError, got %v", code, err)
		}
		if wantReason == "" {
			if sendErr.Fatal {
				t.Fatalf("%s must stay retryable", code)
			}
			continue
		}
		if !sendErr.Fatal || sendErr.Reason != wantReason {
			t.Fatalf("%s: fatal=%v reason=%s, want %s", code, sendErr.Fatal, sendErr.Reason, wantReason)
		}
	}
}

func TestCLISenderNonJSONEnvelopeIsRetryable(t *testing.T) {
	runner := &fakeRunner{envelopes: []string{"not json at all"}}
	sender := &CLISender{options: CLIOptions{}, run: runner.run}
	err := sender.Send(context.Background(), testRegistration("exec-a", PhaseTerminal, 1))
	sendErr, ok := err.(*SendError)
	if !ok {
		t.Fatalf("expected SendError for garbage output, got %v", err)
	}
	if sendErr.Fatal {
		t.Fatal("garbage envelope must not dead-letter")
	}
}
