package adapter

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHermesObservedFailureFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/hermes-gateway-failure.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	p := &processRecord{parser: hermesParser{}, maxOutput: 1 << 20, done: make(chan struct{})}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		p.ingest([]byte(line), false)
	}
	p.finish(nil)
	if p.result == nil || p.result.Success || p.result.Content != "" || p.result.Error != "Hermes reported a failed turn" {
		t.Fatalf("%#v", p.result)
	}
}

func TestHermesTerminalContract(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		success   bool
	}{
		{"answer", `{"type":"result","session_id":"fixture-session","exit_code":0,"text":"  fixture-ok\n"}`, true},
		{"empty", `{"type":"result","exit_code":0,"text":" "}`, false},
		{"error", `{"type":"result","exit_code":0,"text":"bad","error":"private diagnostic"}`, false},
		{"failed", `{"type":"result","exit_code":1,"text":"partial"}`, false},
		{"missing exit", `{"type":"result","text":"partial"}`, false},
		{"fractional exit", `{"type":"result","exit_code":0.5,"text":"partial"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := (hermesParser{}).Parse([]byte(tc.raw), false)
			if !obs.Terminal || obs.Success != tc.success {
				t.Fatalf("%#v", obs)
			}
			if tc.success && (obs.Content != "  fixture-ok\n" || obs.SessionID != "fixture-session") {
				t.Fatalf("%#v", obs)
			}
			if !tc.success && obs.Content != "" {
				t.Fatal("failed content leaked")
			}
		})
	}
	for _, line := range []string{`{"type":"text","text":"partial private answer"}`, `{"type":"tool_result","output":"private output"}`, `{"type":"unknown","text":"bad"}`} {
		obs := (hermesParser{}).Parse([]byte(line), false)
		if obs.Terminal || obs.Content != "" || obs.Summary != "" {
			t.Fatalf("%#v", obs)
		}
	}
	obs := (hermesParser{}).Parse([]byte(`{"type":"result","exit_code":0,"text":"stderr is not an answer"}`), true)
	if obs.Terminal || obs.Content != "" {
		t.Fatal("stderr became answer")
	}
}

func TestHermesManagedStdinAndProcessExit(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		success      bool
	}{
		{"answer", `query=$(cat); test "$query" = 'private prompt' || exit 3
 printf '%s\n' '{"type":"result","session_id":"fixture-session","exit_code":0,"text":"fixture-ok"}'`, true},
		{"late exit", `cat >/dev/null
 printf '%s\n' '{"type":"result","exit_code":0,"text":"discard"}'; exit 1`, false},
		{"delta only", `cat >/dev/null
 printf '%s\n' '{"type":"text","text":"discard"}'`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := fixtureExecutable(t, tc.script)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			got, err := NewHermes().Launch(ctx, LaunchRequest{Argv: []string{exe, "chat", "--format", "stream-json", "--query-file", "-"}, PromptDelivery: PromptDeliveryStdin, Stdin: []byte("private prompt"), DiscoveryWindow: time.Millisecond})
			if err != nil || got.Result == nil || got.Result.Success != tc.success {
				t.Fatalf("%#v %v", got, err)
			}
			if !tc.success && got.Result.Content != "" {
				t.Fatal("partial answer returned")
			}
		})
	}
	for _, req := range []LaunchRequest{
		{Argv: []string{"/missing", "chat", "--format", "stream-json", "--query-file", "-"}},
		{Argv: []string{"/missing", "chat", "--format", "stream-json", "--query-file", "-"}, PromptDelivery: PromptDeliveryStdin, Stdin: []byte{0xff}},
		{Argv: []string{"/missing", "chat"}, PromptDelivery: PromptDeliveryStdin, Stdin: []byte("prompt")},
	} {
		if _, err := NewHermes().Launch(context.Background(), req); err == nil {
			t.Fatal("invalid invocation allowed")
		}
	}
}

func TestHermesResumeDropsCreationOverrides(t *testing.T) {
	base := []string{"hermes", "chat", "--format", "stream-json", "--model", "glm-5.3-flash", "--provider", "custom", "--reasoning", "high", "--max-turns", "2", "--yolo", "--query-file", "-"}
	got, err := ContinuationArgv("hermes", base, PromptDeliveryStdin, "session-2")
	want := []string{"hermes", "chat", "--format", "stream-json", "--resume", "session-2", "--query-file", "-"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v %v", got, err)
	}
	again, err := ContinuationArgv("hermes", got, PromptDeliveryStdin, "session-3")
	if err != nil || again[5] != "session-3" {
		t.Fatalf("%v %v", again, err)
	}
	if cap := NegotiateSteer(hermesManifest(), base, PromptDeliveryStdin, 0); cap.Status != CapabilityUnavailable {
		t.Fatalf("%#v", cap)
	}
}

func TestApprovalRepairForEveryRecipe(t *testing.T) {
	for _, name := range []string{"codex-coding", "codex-read-only", "cursor-coding", "cursor-read-only", "claude-auto", "zcode-yolo", "zcode-read-only", "devin-acp"} {
		t.Run(name, func(t *testing.T) {
			family := strings.Split(name, "-")[0]
			if family == "claude" {
				family = "claude-code"
			}
			p := &processRecord{ref: SourceRef{Adapter: family}, maxOutput: 1 << 20, done: make(chan struct{})}
			p.ingestObservation(parsedObservation{Kind: "attention", State: StateAttention, SourceState: "approval_required", Data: map[string]any{"attention_kind": "approval", "diagnostic_code": "approval_required"}})
			if len(p.events) != 1 || p.events[0].Payload["repair_action"] == nil || p.events[0].Payload["diagnostic_code"] != "approval_required" {
				t.Fatalf("%#v", p.events)
			}
		})
	}
}

func TestNativePolicyRepairExcludesMultica(t *testing.T) {
	p := &processRecord{ref: SourceRef{Adapter: "multica"}, done: make(chan struct{})}
	p.ingestObservation(parsedObservation{Kind: "attention", State: StateAttention, Data: map[string]any{"attention_kind": "approval", "diagnostic_code": "multica_approval_required"}})
	if len(p.events) != 1 || p.events[0].Payload["repair_action"] != nil || p.events[0].Payload["diagnostic_code"] != "multica_approval_required" {
		t.Fatalf("%#v", p.events)
	}
}

func TestHermesRejectsGatewayUnsupportedBudget(t *testing.T) {
	if hermesInvocationSatisfied([]string{"hermes", "chat", "--format", "stream-json", "--query-file", "-", "--run-budget", "30"}) {
		t.Fatal("unsupported gateway budget accepted")
	}
}
