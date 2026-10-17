package adapter

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func acpFixture(t *testing.T, mode string, grant bool) (Adapter, LaunchRequest) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	script, err := filepath.Abs("testdata/devin-acp.py")
	if err != nil {
		t.Fatal(err)
	}
	// argv[1] must be acp, exactly like the real CLI. A tiny wrapper only
	// redirects to the fixture; it never interpolates a prompt or credentials.
	wrapper := filepath.Join(t.TempDir(), "devin")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec '"+python+"' '"+script+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{"ACP_FIXTURE_MODE=" + mode, "AGENTCTL_DEVIN_ACP_APPROVE=0"}
	if grant {
		env = append(env, "AGENTCTL_DEVIN_ACP_APPROVE=1", "ACP_EXPECT_PERMISSION=selected", "ACP_EXPECT_OPTION=yes")
	}
	if mode == "permission-wrong-session" || mode == "permission-no-allow" {
		env = append(env, "ACP_EXPECT_OPTION=no")
	}
	return NewDevin(), LaunchRequest{Argv: []string{wrapper, "acp"}, Stdin: []byte("PRIVATE PROMPT"), PromptDelivery: PromptDeliveryStdin, Env: env, Timeout: 5 * time.Second}
}

func TestDevinACP(t *testing.T) {
	for _, tc := range []struct {
		mode       string
		grant      bool
		state      State
		diagnostic string
	}{
		{"eof", false, StateFailed, "acp_transport_closed_before_result"},
		{"malformed", false, StateFailed, "acp_invalid_message"},
		{"normal", false, StateCompleted, ""}, {"permission", true, StateCompleted, ""}, {"permission", false, StateFailed, "acp_permission_denied"},
		{"permission-no-reject", false, StateFailed, "acp_permission_denied"},
		{"permission-wrong-session", true, StateFailed, "acp_permission_denied"},
		{"permission-no-allow", true, StateFailed, "acp_permission_denied"},
		{"fs", false, StateCompleted, ""}, {"refusal", false, StateFailed, "acp_refusal"}, {"max_tokens", false, StateFailed, "acp_max_tokens"},
		{"max_turn_requests", false, StateFailed, "acp_max_turn_requests"}, {"cancelled", false, StateCancelled, "acp_cancelled"},
		{"future_reason", false, StateFailed, "acp_unknown_stop_reason"}, {"rpc-error", false, StateFailed, "acp_rpc_error"},
	} {
		t.Run(tc.mode+map[bool]string{true: "-grant"}[tc.grant], func(t *testing.T) {
			a, req := acpFixture(t, tc.mode, tc.grant)
			got, err := a.Launch(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Result == nil || got.Result.State != tc.state {
				t.Fatalf("result=%+v", got.Result)
			}
			if got.Session.Ref.OpaqueID != "fixture-session" {
				t.Fatalf("session=%+v", got.Session)
			}
			if tc.state == StateCompleted && got.Result.Content != "Hello world" {
				t.Fatalf("content=%q", got.Result.Content)
			}
			if tc.diagnostic != "" && got.Result.Data["diagnostic_code"] != tc.diagnostic {
				t.Fatalf("data=%v", got.Result.Data)
			}
			events, err := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(events)
			if tc.diagnostic == "acp_permission_denied" && !strings.Contains(string(data), `"kind":"attention"`) {
				t.Fatalf("permission denial missing attention: %s", data)
			}
			for _, private := range []string{"PRIVATE", "Hello", "world", "private tool text", "private-call-id"} {
				if strings.Contains(string(data), private) {
					t.Fatalf("event leaked %s", private)
				}
			}
		})
	}
}
func TestDevinACPLoad(t *testing.T) {
	for _, mode := range []string{"normal", "no-load"} {
		t.Run(mode, func(t *testing.T) {
			a, req := acpFixture(t, mode, false)
			req.Argv = append(req.Argv, devinACPSessionFlag, "fixture-session")
			got, err := a.Launch(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "no-load" {
				if got.Result == nil || got.Result.Data["diagnostic_code"] != "acp_load_session_not_advertised" {
					t.Fatalf("result=%+v", got.Result)
				}
			} else if got.Result == nil || got.Result.Content != "Hello world" {
				t.Fatalf("result=%+v", got.Result)
			}
		})
	}
}
func TestDevinACPCancel(t *testing.T) {
	for _, mode := range []string{"cancel", "cancel-permission"} {
		t.Run(mode, func(t *testing.T) {
			a, req := acpFixture(t, mode, false)
			req.StartOnly = true
			got, err := a.Launch(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Cancel(context.Background(), CancelRequest{Ref: got.Session.Ref, Grace: time.Second}); err != nil {
				t.Fatal(err)
			}
			result, err := a.Result(context.Background(), ResultRequest{Ref: got.Session.Ref})
			if err != nil {
				t.Fatal(err)
			}
			if result.State != StateCancelled || result.Data["diagnostic_code"] != "acp_cancelled" {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

// Opt-in because it invokes the authenticated provider, creates a native
// session, and consumes a small real prompt. Missing installation/auth skips.
func TestDevinACPRealSmoke(t *testing.T) {
	if os.Getenv("AGENTCTL_TEST_DEVIN_ACP") != "1" {
		t.Skip("set AGENTCTL_TEST_DEVIN_ACP=1 for authenticated smoke")
	}
	bin, err := exec.LookPath("devin")
	if err != nil {
		t.Skip("devin not installed")
	}
	a := NewDevin()
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{bin, "acp"}, Stdin: []byte("Reply with ACP_SMOKE_OK. Do not call tools or change files."), PromptDelivery: PromptDeliveryStdin, Timeout: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != nil && got.Result.Data["native_code"] == -32000 {
		t.Skip("Devin session unavailable: authentication/provider readiness not established")
	}
	if got.Result == nil || !got.Result.Success || got.Session.Ref.OpaqueID == "" {
		t.Fatalf("smoke result=%+v", got.Result)
	}
	next, err := a.Resume(context.Background(), ResumeRequest{Ref: got.Session.Ref, Argv: []string{bin, "acp"}, PromptDelivery: PromptDeliveryStdin, Prompt: []byte("Repeat the exact marker from the previous turn. Do not call tools."), Timeout: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if next.Result == nil || !next.Result.Success || !strings.Contains(next.Result.Content, "ACP_SMOKE_OK") || next.Session.Ref.OpaqueID != got.Session.Ref.OpaqueID {
		t.Fatalf("follow-up=%+v", next.Result)
	}
}

func TestDevinACPAmbientGrantDoesNotApprove(t *testing.T) {
	t.Setenv("AGENTCTL_DEVIN_ACP_APPROVE", "1")
	a, req := acpFixture(t, "permission", false)
	req.Env = []string{"ACP_FIXTURE_MODE=permission"}
	got, err := a.Launch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Success || got.Result.Data["diagnostic_code"] != "acp_permission_denied" {
		t.Fatalf("result=%+v", got.Result)
	}
}

func TestDevinACPCancelEscalation(t *testing.T) {
	a, req := acpFixture(t, "cancel-hang", false)
	req.StartOnly = true
	got, err := a.Launch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Cancel(context.Background(), CancelRequest{Ref: got.Session.Ref, Grace: 50 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	result, err := a.Result(context.Background(), ResultRequest{Ref: got.Session.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateCancelled || result.Success {
		t.Fatalf("result=%+v", result)
	}
}

func TestDevinACPFinalMessageAfterToolActivity(t *testing.T) {
	a, req := acpFixture(t, "interleaved", false)
	got, err := a.Launch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.Content != "FILES=4" || got.Result.Summary != "FILES=4" {
		t.Fatalf("result=%+v", got.Result)
	}
	if got.Result.Data["result_content_source"] != "assistant_terminal_result" {
		t.Fatalf("source=%v", got.Result.Data)
	}
}
