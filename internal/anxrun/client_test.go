package anxrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
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
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "api agents.me.get") {
		return []byte(`{"ok":true,"result":{"agent":{"actor_id":"actor-test"}}}`), nil
	}
	if strings.Contains(joined, "api cards.get") {
		return []byte(`{"ok":true,"result":{"card":{"thread_id":"thread-test"}}}`), nil
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
	if len(runner.argvs) != 8 {
		t.Fatalf("invocations = %d (session + 2 participants + identity + 2 card reads + 2 messages)", len(runner.argvs))
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

func TestCLISenderNoSessionStillRegistersRun(t *testing.T) {
	runner := &fakeRunner{}
	sender := &CLISender{options: CLIOptions{}, run: runner.run}
	registration := testRegistration("exec-a", PhaseTerminal, 1)
	registration.NativeSession = nil
	registration.State = StateCompleted
	registration.Outcome = &Outcome{Availability: AvailabilityUnavailableAtSource, Links: []Link{{Kind: LinkKindPR, Ref: "https://github.com/o/r/pull/104"}}}
	if err := sender.Send(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	if len(runner.argvs) != 3 {
		t.Fatalf("no-session calls: %v", runner.argvs)
	}
	if !strings.Contains(strings.Join(runner.argvs[2], " "), "api events.create") {
		t.Fatal("run record never transmitted")
	}
	var body struct {
		RequestKey string `json:"request_key"`
		Event      struct {
			Payload struct {
				Text string `json:"text"`
			} `json:"payload"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(runner.stdins[2]), &body); err != nil {
		t.Fatal(err)
	}
	var got Registration
	if err := json.Unmarshal([]byte(body.Event.Payload.Text), &got); err != nil {
		t.Fatal(err)
	}
	if got.DurableID != registration.DurableID || got.State != StateCompleted || got.Outcome == nil || len(got.Outcome.Links) != 1 || body.RequestKey == "" {
		t.Fatalf("incomplete run registration: %+v", got)
	}
}

func TestCLISenderMulticaWithoutStoredOutcome(t *testing.T) {
	execution := testExecution(t, []string{"anx.card.dogfooding"}, model.StateCompleted)
	execution.Authority = model.AuthorityMultica
	r := BuildTerminal(execution, nil, false, time.Now())
	if r.NativeSession != nil || r.Outcome == nil || r.Outcome.Availability != AvailabilityUnavailableAtSource {
		t.Fatalf("false Multica availability: %+v", r)
	}
	runner := &fakeRunner{}
	if err := (&CLISender{run: runner.run}).Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(runner.argvs) != 3 || !strings.Contains(runner.stdins[2], AvailabilityUnavailableAtSource) {
		t.Fatalf("Multica run outcome not delivered: %v", runner.argvs)
	}
}

func TestCLISenderOnlyDeletesAfterRunRecordAcknowledged(t *testing.T) {
	spool := testSpool(t)
	r := testRegistration("exec-a", PhaseTerminal, 0)
	r.State = StateCompleted
	r.RequestKey = "private-key"
	r.Outcome = &Outcome{Availability: AvailabilityStored, Summary: "Passed tests.", Links: []Link{{Kind: LinkKindPR, Ref: "https://github.com/o/r/pull/104"}}}
	if _, err := spool.Enqueue(r); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	var firstBody string
	failMessage := true
	sender := &CLISender{options: CLIOptions{}, run: func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
		if strings.Contains(strings.Join(argv, " "), "api events.create") {
			if firstBody == "" {
				firstBody = string(stdin)
			} else if firstBody != string(stdin) {
				t.Error("retry changed idempotency key or payload")
			}
			if strings.Contains(string(stdin), "private-key") || strings.Contains(string(stdin), "native_session") || strings.Contains(string(stdin), "fingerprint") {
				t.Error("private coordinates escaped into card message")
			}
			if failMessage {
				return []byte(`{"ok":false,"error":{"code":"sessions_unavailable","message":"private failure text"}}`), nil
			}
		}
		return runner.run(ctx, argv, stdin)
	}}
	result, err := spool.Flush(context.Background(), sender, 0)
	if err != nil || result.Retried != 1 || result.Delivered != 0 || firstBody == "" {
		t.Fatalf("missing run-record delivery: %+v %v", result, err)
	}
	entries, _ := spool.ListPending()
	if len(entries) != 1 {
		t.Fatal("partial delivery removed registration")
	}
	entries[0].NextAttemptAt = time.Time{}
	if err := rewriteEntry(spool, entries[0]); err != nil {
		t.Fatal(err)
	}
	failMessage = false
	result, err = spool.Flush(context.Background(), sender, 0)
	if err != nil || result.Delivered != 1 {
		t.Fatalf("complete delivery failed: %+v %v", result, err)
	}
	if entries, _ := spool.ListPending(); len(entries) != 0 {
		t.Fatal("acknowledged registration stayed pending")
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
