package anxrun

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

// TestNativeSessionFingerprintMatchesIdentityV1 pins the fingerprint to the
// identity v1 correlator: SHA-256 over each component followed by a NUL byte,
// including the final native session id (docs/identity.md; the design doc's
// original formula omitted that trailing NUL — review thread 1 on PR #104).
func TestNativeSessionFingerprintMatchesIdentityV1(t *testing.T) {
	provider, session := "claude", "4dba0f53-9021-4c86-a701-30e1c8c657cd"
	h := sha256.New()
	for _, part := range []string{"agentctl.identity.v1", "native_session", provider, session} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	want := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got := NativeSessionFingerprint(provider, session); got != want {
		t.Fatalf("fingerprint diverged from identity v1 formula: got %s want %s", got, want)
	}
	if got := NativeSessionFingerprint(provider, session); got != adapter.Fingerprint("agentctl.identity.v1", "native_session", provider, session) {
		t.Fatal("fingerprint does not reuse adapter.Fingerprint output")
	}
	if NativeSessionFingerprint("codex", session) == want {
		t.Fatal("fingerprint must change with provider")
	}
}

func TestCanonicalProvider(t *testing.T) {
	for input, want := range map[string]string{"claude-code": "claude", "generic-process": "generic", "codex": "codex", "zcode": "zcode", "hermes": "hermes"} {
		if got := CanonicalProvider(input); got != want {
			t.Fatalf("CanonicalProvider(%q) = %q, want %q", input, got, want)
		}
	}
}

func testExecution(t *testing.T, labels []string, state model.State) model.Execution {
	t.Helper()
	now := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)
	executionID, err := ids.ParseExecutionID("exec-calm-pen-slide-adjust-steak-antique")
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := ids.ParseHostID("host-goose-addict-spray-hawk-lens-horror")
	if err != nil {
		t.Fatal(err)
	}
	session := "4dba0f53-9021-4c86-a701-30e1c8c657cd"
	started := now
	terminal := now.Add(25 * time.Minute)
	execution := model.Execution{
		SchemaVersion: model.SchemaVersion, ID: executionID, OriginHostID: hostID, Revision: 1,
		Authority: model.AuthorityNative, Adapter: "claude-code", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched,
		State: state, Liveness: model.LivenessAlive, Labels: labels,
		SourceBindings: []model.SourceBinding{{Kind: nativeSessionBindingKind, Fingerprint: "sha256:" + strings.Repeat("0", 64), OpaqueID: &session}},
		Capabilities:   model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		StartedAt:      &started, TerminalAt: &terminal,
		Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now},
	}
	return execution
}

func TestBuildStartAndTerminal(t *testing.T) {
	now := time.Date(2026, 10, 24, 6, 30, 0, 0, time.UTC)
	execution := testExecution(t, []string{"anx.card.watch-redesign", "review"}, model.StateRunning)
	execution.Delegation = &model.DelegationBinding{RequestSHA256: "sha256:" + hex.EncodeToString(make([]byte, 32)), ConfigurationSHA256: "sha256:" + hex.EncodeToString(make([]byte, 32)), Requested: []byte(`{"family":"claude"}`), RequestKey: "watch-redesign-lane-a-1", Resolved: model.DelegationTarget{Harness: "claude-code", Model: "claude-opus-5-5", Host: "m5", Authority: model.AuthorityNative}}

	start := BuildStart(execution, now)
	if start.Phase != PhaseStart || start.Launcher != "agentctl" || start.ExternalID != execution.ID.String() || start.OriginHostID != execution.OriginHostID.String() {
		t.Fatalf("start identity wrong: %#v", start)
	}
	if start.DurableID != "agentctl://"+execution.OriginHostID.String()+"/"+execution.ID.String() {
		t.Fatalf("durable id = %q", start.DurableID)
	}
	if start.RequestKey != "watch-redesign-lane-a-1" {
		t.Fatalf("request key = %q", start.RequestKey)
	}
	if start.Model != "claude-opus-5-5" {
		t.Fatalf("model = %q", start.Model)
	}
	if len(start.CardRefs) != 1 || start.CardRefs[0] != "card:watch-redesign" {
		t.Fatalf("card refs = %#v", start.CardRefs)
	}
	if start.NativeSession == nil || start.NativeSession.Provider != "claude" || start.NativeSession.Kind != NativeSessionIDKind {
		t.Fatalf("native session = %#v", start.NativeSession)
	}
	if start.NativeSession.Fingerprint != NativeSessionFingerprint("claude", "4dba0f53-9021-4c86-a701-30e1c8c657cd") {
		t.Fatalf("native session fingerprint = %q", start.NativeSession.Fingerprint)
	}

	terminal := execution
	terminal.State = model.StateCompleted
	terminal.Liveness = model.LivenessExited
	outcome := model.Outcome{SchemaVersion: 1, ExecutionID: execution.ID, Revision: 1, State: model.StateCompleted, Availability: model.OutcomeStored, RecordedAt: now, Source: "claude-code", ResultRef: start.DurableID,
		Content: &model.OutcomeContent{MediaType: "text/plain", Text: "Landed the watcher.\ncommit: 0123456789abcdef0123456789abcdef01234567", Preview: "Landed", Bytes: 68}}
	done := BuildTerminal(terminal, &outcome, false, now)
	if done.Phase != PhaseTerminal || done.State != StateCompleted || done.Liveness != LivenessStale {
		t.Fatalf("terminal projection wrong: %#v", done)
	}
	if done.ResultCollected {
		t.Fatal("uncollected result reported collected")
	}
	if done.Outcome == nil || done.Outcome.Summary == "" || len(done.Outcome.Links) != 1 || done.Outcome.Links[0].Kind != LinkKindCommit {
		t.Fatalf("outcome projection = %#v", done.Outcome)
	}
	if done.EndedAt == nil || !done.EndedAt.Equal(*terminal.TerminalAt) {
		t.Fatalf("ended_at = %#v", done.EndedAt)
	}
}

func TestANXStateAndLivenessMapping(t *testing.T) {
	for _, row := range []struct {
		state model.State
		want  string
	}{
		{model.StateCreated, StateStarting},
		{model.StateRunning, StateRunning},
		{model.StateAttention, StateRunning},
		{model.StateCompleted, StateCompleted},
		{model.StateFailed, StateFailed},
		{model.StateCancelled, StateCancelled},
		{model.StateOrphaned, StateUnknown},
	} {
		if got := ANXState(row.state); got != row.want {
			t.Fatalf("ANXState(%s) = %s, want %s", row.state, got, row.want)
		}
	}
	if got := ANXLiveness(model.LivenessAlive); got != LivenessAlive {
		t.Fatalf("alive liveness = %s", got)
	}
	if got := ANXLiveness(model.LivenessExited); got != LivenessStale {
		t.Fatalf("exited liveness = %s", got)
	}
}

func TestNativeSessionAbsentForMulticaAndUnbound(t *testing.T) {
	multica := testExecution(t, []string{"anx.card.x"}, model.StateRunning)
	multica.Authority = model.AuthorityMultica
	if got := nativeSession(multica); got != nil {
		t.Fatalf("multica execution has a native session: %#v", got)
	}
	unbound := testExecution(t, nil, model.StateRunning)
	unbound.SourceBindings = nil
	if got := nativeSession(unbound); got != nil {
		t.Fatalf("unbound execution has a native session: %#v", got)
	}
}

func TestResolveCards(t *testing.T) {
	cards := ResolveCards([]string{"anx.card.b", "review", "anx.card.a", "anx.card.a", "anx."})
	if len(cards) != 2 || cards[0] != "card:a" || cards[1] != "card:b" {
		t.Fatalf("cards = %#v", cards)
	}
	if Labeled([]string{"review"}) || !Labeled([]string{"anx.card.x"}) {
		t.Fatal("Labeled misbehaves")
	}
}

func TestParseCardOverride(t *testing.T) {
	for input, want := range map[string]string{
		"card:watch-redesign": "anx.card.watch-redesign",
		"watch-redesign":      "anx.card.watch-redesign",
		" card:trim ":         "anx.card.trim",
	} {
		got, err := ParseCardOverride(input)
		if err != nil || got != want {
			t.Fatalf("ParseCardOverride(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, bad := range []string{"", "card:", "Card:Upper", "has space"} {
		if _, err := ParseCardOverride(bad); err == nil {
			t.Fatalf("ParseCardOverride(%q) unexpectedly accepted", bad)
		}
	}
}
