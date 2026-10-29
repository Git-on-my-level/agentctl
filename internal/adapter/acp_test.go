package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	script, err := filepath.Abs("testdata/acp-agent.py")
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
func TestACPConfigVerification(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		config     []string
		diagnostic string
		want       map[string]any
	}{
		{name: "already-current", config: []string{"model=swe-2-high"}, want: map[string]any{"model": "swe-2-high"}},
		{name: "set-and-read-back", config: []string{"model=gpt-6-luna-medium", "repos=owner/repo"}, want: map[string]any{"model": "gpt-6-luna-medium", "repos": "owner/repo"}},
		{name: "ignored-set", mode: "ignore", config: []string{"devin_version=devin-swe-2-high"}, diagnostic: "acp_config_unverified"},
		{name: "rejected-set", mode: "reject", config: []string{"model=gpt-6-luna-medium"}, diagnostic: "acp_config_rejected"},
		{name: "unknown-option", config: []string{"thought_level=max"}, diagnostic: "acp_config_option_unavailable"},
		{name: "unlisted-value", config: []string{"model=gemini-4-argon-high"}, diagnostic: "acp_config_value_unavailable"},
		{name: "options-absent", mode: "absent", config: []string{"model=swe-2-high"}, diagnostic: "acp_config_option_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, req := acpFixture(t, "normal", false)
			req.Env = append(req.Env, "ACP_CONFIG_MODE="+tc.mode)
			for _, pair := range tc.config {
				req.Argv = append(req.Argv, acpConfigFlag, pair)
			}
			got, err := a.Launch(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if tc.diagnostic != "" {
				if got.Result == nil || got.Result.Success || got.Result.Data["diagnostic_code"] != tc.diagnostic {
					t.Fatalf("result=%+v", got.Result)
				}
				if tc.diagnostic != "acp_config_rejected" {
					mismatch, _ := got.Result.Data["acp_config_mismatch"].(map[string]any)
					if mismatch["config_id"] == nil || mismatch["requested"] == nil {
						t.Fatalf("mismatch=%v", got.Result.Data)
					}
				}
				return
			}
			if got.Result == nil || !got.Result.Success || got.Result.Content != "Hello world" {
				t.Fatalf("result=%+v", got.Result)
			}
			if verified, _ := json.Marshal(got.Result.Data["acp_config"]); string(verified) != string(mustJSON(t, tc.want)) {
				t.Fatalf("acp_config=%s", verified)
			}
			events, err := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(events)
			if !strings.Contains(string(data), `"source_state":"acp.config"`) {
				t.Fatalf("config event missing: %s", data)
			}
		})
	}
}

func TestACPConfigFlagsNeverReachNativeArgv(t *testing.T) {
	argv := []string{"devin", "acp", acpConfigFlag, "model=swe-2-high", acpSessionFlag, "s1", "--model", "swe-2-high"}
	if got := strings.Join(acpExecArgv(argv), " "); got != "devin acp --model swe-2-high" {
		t.Fatalf("exec argv=%s", got)
	}
	for _, bad := range [][]string{{"devin", "acp", acpConfigFlag, "model"}, {"devin", "acp", acpConfigFlag, "=x"}, {"devin", "acp", acpConfigFlag, "a=1", acpConfigFlag, "a=2"}} {
		if err := ValidateInvocationPrompt("devin", bad, PromptDeliveryStdin, []byte("task")); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestDevinPlacementIsExact(t *testing.T) {
	for _, tc := range []struct {
		adapter    string
		argv       []string
		diagnostic string
	}{
		{"devin", []string{"devin", "acp", "--cloud"}, "devin_cloud_requires_cloud_adapter"},
		{"devin", []string{"devin", "--cloud", "-p", "x"}, "devin_cloud_requires_cloud_adapter"},
		{"devin-cloud", []string{"devin", "acp"}, "devin_cloud_requires_acp"},
		{"devin-cloud", []string{"devin", "--cloud", "-p", "x"}, "devin_cloud_requires_acp"},
		{"devin", []string{"devin", "acp"}, ""},
		{"devin-cloud", []string{"devin", "acp", "--cloud"}, ""},
	} {
		err := ValidateInvocationPrompt(tc.adapter, tc.argv, PromptDeliveryStdin, []byte("task"))
		var issue *AdapterError
		if tc.diagnostic == "" {
			if err != nil {
				t.Fatalf("%s %v: %v", tc.adapter, tc.argv, err)
			}
			continue
		}
		if !errors.As(err, &issue) || issue.Details["diagnostic_code"] != tc.diagnostic {
			t.Fatalf("%s %v: err=%v", tc.adapter, tc.argv, err)
		}
	}
	if m := NewDevinCloud().Manifest(); m.Placement != PlacementCloud || m.Adapter != "devin-cloud" || m.StructuredOutput.Format != "acp-jsonrpc" {
		t.Fatalf("cloud manifest=%+v", m)
	}
	if m := NewDevin().Manifest(); m.Placement != "" {
		t.Fatalf("local placement=%q", m.Placement)
	}
}

// withSettle shortens one profile's post-end_turn settle window for a test.
func withSettle(t *testing.T, adapterName string, settle time.Duration) {
	t.Helper()
	original := acpProfiles[adapterName]
	changed := original
	changed.settle = settle
	acpProfiles[adapterName] = changed
	t.Cleanup(func() { acpProfiles[adapterName] = original })
}

func TestACPSettlesForTrailingMessage(t *testing.T) {
	withSettle(t, "devin-cloud", 300*time.Millisecond)
	for _, tc := range []struct {
		adapter, mode, content string
		url                    bool
	}{
		{"devin-cloud", "trailing", "Hello world", true},
		{"devin-cloud", "trailing-split", "Hello world", true},
		{"devin-cloud", "normal", "Hello world", false},
		// Local Devin does not settle; a late message is not waited for.
		{"devin", "trailing", "", false},
	} {
		t.Run(tc.adapter+"-"+tc.mode, func(t *testing.T) {
			_, req := acpFixture(t, tc.mode, false)
			a := NewDevin()
			if tc.adapter == "devin-cloud" {
				a = NewDevinCloud()
				req.Argv = append(req.Argv, "--cloud")
			}
			got, err := a.Launch(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Result == nil || got.Result.State != StateCompleted || got.Result.Content != tc.content {
				t.Fatalf("result=%+v", got.Result)
			}
			if url, _ := got.Result.Data["session_url"].(string); (url == "https://app.devin.ai/sessions/abc123") != tc.url {
				t.Fatalf("session_url=%q", url)
			}
		})
	}
	// Silence after end_turn is not an answer.
	_, req := acpFixture(t, "silent", false)
	req.Argv = append(req.Argv, "--cloud")
	silent, err := NewDevinCloud().Launch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if silent.Result == nil || silent.Result.Success || silent.Result.State != StateFailed || silent.Result.Data["diagnostic_code"] != "acp_answer_missing" || silent.Result.Data["session_url"] != "https://app.devin.ai/sessions/abc123" {
		t.Fatalf("silent=%+v", silent.Result)
	}
	if url := devinCloudSessionURL([]byte(`{"cognition.ai/session":{"cognition.ai/url":"https://evil.example/sessions/x"}}`)); url != "" {
		t.Fatalf("accepted %q", url)
	}
}

func TestACPCancelDuringSettleIsCancelled(t *testing.T) {
	withSettle(t, "devin-cloud", 10*time.Second)
	a, req := acpFixture(t, "settle-cancel", false)
	a = NewDevinCloud()
	req.Argv = append(req.Argv, "--cloud")
	req.StartOnly = true
	got, err := a.Launch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Wait until end_turn has been received and the turn is settling.
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, _ := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
		data, _ := json.Marshal(events)
		if strings.Contains(string(data), "acp.tool_call") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if err := a.Cancel(context.Background(), CancelRequest{Ref: got.Session.Ref, Grace: 200 * time.Millisecond}); err != nil {
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

func TestACPCancelDuringConfigureSendsNoPrompt(t *testing.T) {
	a, req := acpFixture(t, "cancel-config", false)
	marker := filepath.Join(t.TempDir(), "prompted")
	req.Env = append(req.Env, "ACP_PROMPT_MARKER="+marker)
	req.Argv = append(req.Argv, acpConfigFlag, "model=gpt-6-luna-medium")
	req.StartOnly = true
	got, err := a.Launch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, _ := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
		data, _ := json.Marshal(events)
		if strings.Contains(string(data), "acp.session") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
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
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("prompt was sent after cancellation")
	}
}

func TestDevinCloudACPFixture(t *testing.T) {
	withSettle(t, "devin-cloud", 100*time.Millisecond)
	_, req := acpFixture(t, "normal", false)
	req.Argv = append(req.Argv, "--cloud", acpConfigFlag, "devin_version=devin-swe-2-high", acpConfigFlag, "repos=owner/repo")
	a := NewDevinCloud()
	got, err := a.Launch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Session.Ref.OpaqueID != "fixture-session" {
		t.Fatalf("result=%+v", got.Result)
	}
	if verified := got.Result.Data["acp_config"].(map[string]any); verified["devin_version"] != "devin-swe-2-high" || verified["repos"] != "owner/repo" {
		t.Fatalf("acp_config=%v", verified)
	}
	next, err := a.Resume(context.Background(), ResumeRequest{Ref: got.Session.Ref, Argv: req.Argv, PromptDelivery: PromptDeliveryStdin, Prompt: []byte("again"), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if next.Result == nil || !next.Result.Success || next.Result.Data["acp_config"] == nil {
		t.Fatalf("continue=%+v", next.Result)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDevinACPLoad(t *testing.T) {
	for _, mode := range []string{"normal", "no-load"} {
		t.Run(mode, func(t *testing.T) {
			a, req := acpFixture(t, mode, false)
			req.Argv = append(req.Argv, acpSessionFlag, "fixture-session")
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

func TestDevinACPPromptNegotiation(t *testing.T) {
	argv := []string{"devin", "acp"}
	for _, name := range []CapabilityName{CapabilityLaunch, CapabilityResultContent} {
		for _, tc := range []struct {
			prompt []InvocationPrompt
			want   CapabilityStatus
		}{
			{nil, CapabilityUnavailable},
			{[]InvocationPrompt{{Delivery: PromptDeliveryArgv, Bytes: 4}}, CapabilityUnavailable},
			{[]InvocationPrompt{{Delivery: PromptDeliveryStream, Bytes: 4}}, CapabilityUnavailable},
			{[]InvocationPrompt{{Delivery: PromptDeliveryStdin}}, CapabilityUnavailable},
			{[]InvocationPrompt{{Delivery: PromptDeliveryStdin, Bytes: 4}}, CapabilitySupported},
		} {
			got := NegotiateInvocation(devinManifest(), argv, name, tc.prompt...)
			if got.Status != tc.want {
				t.Fatalf("capability=%s prompt=%+v got=%+v", name, tc.prompt, got)
			}
		}
	}
}

func TestDevinACPLaunchValidatesPromptBeforeStartingProcess(t *testing.T) {
	for _, tc := range []struct {
		name, delivery string
		prompt         []byte
		diagnostic     string
	}{
		{"argv", PromptDeliveryArgv, []byte("task"), "acp_prompt_delivery_required"},
		{"missing", "", nil, "acp_prompt_delivery_required"},
		{"empty", PromptDeliveryStdin, nil, "acp_prompt_required"},
		{"invalid-utf8", PromptDeliveryStdin, []byte{0xff}, "acp_prompt_invalid_utf8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nonexistent executable proves validation happens before exec.Start.
			a := NewDevin()
			_, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{filepath.Join(t.TempDir(), "missing-devin"), "acp"}, PromptDelivery: tc.delivery, Stdin: tc.prompt})
			var issue *AdapterError
			if !errors.As(err, &issue) || issue.Details["diagnostic_code"] != tc.diagnostic {
				t.Fatalf("err=%+v", err)
			}
		})
	}
}

func TestDevinACPProbeRequiresPromptEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho fixture-version\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var fingerprints []string
	for _, size := range []int{0, 4} {
		probe, err := NewDevin().Probe(context.Background(), ProbeRequest{Executable: path, Argv: []string{path, "acp"}, PromptDelivery: PromptDeliveryStdin, PromptBytes: size})
		if err != nil {
			t.Fatal(err)
		}
		fingerprints = append(fingerprints, probe.ScopeFingerprint)
		for _, cap := range probe.Capabilities {
			if cap.Name == CapabilityLaunch || cap.Name == CapabilityResultContent {
				want := CapabilitySupported
				if size == 0 {
					want = CapabilityUnavailable
				}
				if cap.Status != want {
					t.Fatalf("prompt bytes=%d capability=%+v", size, cap)
				}
			}
		}
	}
	if fingerprints[0] == fingerprints[1] {
		t.Fatal("probe scope must distinguish prompt evidence")
	}
}

func TestACPCancelEndsABlockedPromptWrite(t *testing.T) {
	a, req := acpFixture(t, "deaf", false)
	// Far beyond any pipe buffer: the write blocks once the server stops reading.
	req.Stdin = bytes.Repeat([]byte("p"), 8<<20)
	req.StartOnly = true
	// Launch, status reads, cancel and the result must all stay live while the
	// prompt write is blocked.
	done := make(chan Result, 1)
	go func() {
		got, err := a.Launch(context.Background(), req)
		if err != nil {
			done <- Result{State: StateFailed}
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			events, _ := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
			data, _ := json.Marshal(events)
			if strings.Contains(string(data), "acp.session") || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		_ = a.Cancel(context.Background(), CancelRequest{Ref: got.Session.Ref, Grace: 200 * time.Millisecond})
		result, _ := a.Result(context.Background(), ResultRequest{Ref: got.Session.Ref})
		done <- result
	}()
	select {
	case result := <-done:
		if result.State != StateCancelled || result.Success {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a launch blocked writing its prompt stayed uncancellable")
	}
}
