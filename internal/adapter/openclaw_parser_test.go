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
