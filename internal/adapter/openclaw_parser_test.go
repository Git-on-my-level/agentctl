package adapter

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestOpenClawParser(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want, state string
	}{
		{"answer", `{"payloads":[{"text":"First"},{"text":"Second"}],"meta":{"sessionId":"session-1"}}`, "First\n\nSecond", "completed"},
		{"whitespace preserved", `{"payloads":[{"text":"  first\n"},{"text":" \t "},{"text":"\tsecond  \n"}]}`, "  first\n\n\n\tsecond  \n", "completed"},
		{"whitespace only", `{"payloads":[{"text":" \n\t "}]}`, "", "failed"},
		{"empty", `{"payloads":[]}`, "", "failed"},
		{"error", `{"payloads":[{"text":"bad token","isError":true}]}`, "", "failed"},
		{"in_flight", `{"status":"in_flight","payloads":[]}`, "", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := openclawParser{}.Parse([]byte(tc.raw), false)
			if string(obs.State) != tc.state || obs.Content != tc.want || !obs.Terminal {
				t.Fatalf("observation = %#v", obs)
			}
			if tc.name == "answer" && (obs.SessionID != "session-1" || obs.ContentSource != "assistant_terminal_result") {
				t.Fatalf("answer provenance = %#v", obs)
			}
		})
	}
}

func TestOpenClawExitAndSessionIsolation(t *testing.T) {
	good := fixtureExecutable(t, `printf '%s\n' '{"payloads":[{"text":"SMOKE-OK"}]}'`)
	a := NewOpenClaw()
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{good, "agent", "--local", "--agent", "main", "--json", "-m", "hello"}, DiscoveryWindow: time.Second})
	if err != nil || got.Result == nil || !got.Result.Success || got.Result.Content != "SMOKE-OK" {
		t.Fatalf("launch = %#v err=%v", got, err)
	}
	bad := fixtureExecutable(t, `printf '%s\n' '{"payloads":[{"text":"partial"}]}'; printf '%s\n' 'OAuthRefreshFailureError: invalid refresh token' >&2; exit 1`)
	got, err = a.Launch(context.Background(), LaunchRequest{Argv: []string{bad, "agent", "--local", "--agent", "main", "--json", "-m", "hello"}, DiscoveryWindow: time.Second})
	if err != nil || got.Result == nil || got.Result.Success || got.Result.State != StateFailed || got.Result.Content != "" || got.Result.Summary != "" || got.Result.ContentType != "" || got.Result.ContentTruncated || !strings.Contains(got.Result.Error, "OAuthRefreshFailureError") {
		t.Fatalf("failed launch = %#v err=%v", got, err)
	}
	if _, ok := got.Result.Data["result_content_source"]; ok {
		t.Fatalf("failed launch kept discarded answer provenance: %#v", got.Result.Data)
	}
	one := rewriteOpenClawArgv([]string{"openclaw", "agent", "--local", "--agent", "ops", "--json", "-m", "hi"})
	two := rewriteOpenClawArgv([]string{"openclaw", "agent", "--local", "--agent", "ops", "--json", "-m", "hi"})
	if len(one) != len(two) || one[3] == two[3] || !strings.HasPrefix(one[3], "agent:ops:agentctl-") {
		t.Fatalf("session keys are not isolated: %v %v", one, two)
	}
	explicit := rewriteOpenClawArgv([]string{"openclaw", "agent", "--local", "--session-key", "agent:ops:existing", "--json"})
	if len(explicit) != 6 || explicit[4] != "agent:ops:existing" {
		t.Fatalf("explicit session was changed: %v", explicit)
	}
	if _, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{good, "agent", "--json", "-m", "hello"}}); err == nil {
		t.Fatal("gateway invocation was accepted")
	}
}

func TestOpenClawInvocationOptionStructure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		valid bool
	}{
		{"missing prompt", []string{"--local", "--json"}, false},
		{"missing value", []string{"--local", "--json", "-m"}, false},
		{"empty prompt", []string{"--local", "--json", "-m", ""}, false},
		{"blank prompt", []string{"--local", "--json", "--message=  "}, false},
		{"json as message", []string{"--local", "-m", "--json"}, false},
		{"local as message", []string{"--json", "-m", "--local"}, false},
		{"json as model", []string{"--local", "--model", "--json", "-m", "hi"}, false},
		{"local as agent", []string{"--agent", "--local", "--json", "-m", "hi"}, false},
		{"unknown option", []string{"--unknown", "--json", "--local", "-m", "hi"}, false},
		{"delimiter", []string{"--local", "--", "--json", "-m", "hi"}, false},
		{"false json", []string{"--local", "--json=false", "-m", "hi"}, false},
		{"duplicate message", []string{"--local", "--json", "-m", "hi", "--message="}, false},
		{"short message", []string{"--local", "--json", "-m", "hi"}, true},
		{"long message", []string{"--local", "--json", "--message", "hi"}, true},
		{"inline message", []string{"--local", "--json", "--message=hi"}, true},
		{"flag text in message", []string{"--local", "--json", "-m", "--session-key"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := append([]string{"openclaw", "agent"}, tc.args...)
			if got := openclawInvocationSatisfied(argv); got != tc.valid {
				t.Fatalf("satisfied = %v, want %v", got, tc.valid)
			}
			capability := NegotiateInvocation(openclawManifest(), argv, CapabilityResultContent)
			if (capability.Status != CapabilityUnavailable) != tc.valid {
				t.Fatalf("negotiation = %#v", capability)
			}
			if !tc.valid {
				argv[0] = "/nonexistent/openclaw"
				if _, err := NewOpenClaw().Launch(context.Background(), LaunchRequest{Argv: argv}); err == nil {
					t.Fatal("invalid invocation admitted")
				}
			}
		})
	}
	// Flag-looking prompt text must not suppress the fresh session key.
	argv := rewriteOpenClawArgv([]string{"openclaw", "agent", "--local", "--json", "-m", "--session-key"})
	if len(argv) != 8 || argv[2] != "--session-key" {
		t.Fatalf("missing isolated session: %v", argv)
	}
}

func TestOpenClawCancelDiscardsPrintedAnswer(t *testing.T) {
	p := &processRecord{
		parser: openclawParser{}, cancelled: true, done: make(chan struct{}),
		result: &Result{Success: true, State: StateCompleted, Content: "partial", Summary: "partial", ContentType: "text/plain", Data: map[string]any{"result_content_source": "assistant_terminal_result"}},
	}
	p.finish(nil)
	if p.result.State != StateCancelled || p.result.Success || p.result.Content != "" || p.result.Summary != "" || p.result.ContentType != "" {
		t.Fatalf("cancelled result = %#v", p.result)
	}
	if _, exists := p.result.Data["result_content_source"]; exists {
		t.Fatal("cancelled result retains answer provenance")
	}
}

func TestOpenClawCapabilitiesHaveUniqueNames(t *testing.T) {
	seen := map[CapabilityName]bool{}
	for _, declaration := range NewOpenClaw().Manifest().Capabilities {
		if seen[declaration.Name] {
			t.Fatalf("duplicate capability %s", declaration.Name)
		}
		seen[declaration.Name] = true
	}
}
