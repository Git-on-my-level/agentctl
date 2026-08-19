package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureExecutable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGenericLaunchDiscoversSessionAndStructuredSuccess(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"session.started","session_id":"fixture-session"}' '{"type":"result","result":"bounded summary","is_error":false}'`)
	a := NewGenericProcess()
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Session.Ref.OpaqueID != "fixture-session" {
		t.Fatalf("session discovery = %q", got.Session.Ref.OpaqueID)
	}
	if got.Result == nil || !got.Result.Success || got.Result.State != StateCompleted {
		t.Fatalf("unexpected result: %#v", got.Result)
	}
	if got.Session.Binding.Fingerprint == "" || !strings.HasPrefix(got.Session.Binding.Fingerprint, "sha256:") {
		t.Fatalf("missing source fingerprint: %#v", got.Session.Binding)
	}
	events, err := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "started" || events[1].Kind != "terminal" {
		t.Fatalf("unexpected events: %#v", events)
	}
	if events[0].DedupeKey == events[1].DedupeKey {
		t.Fatal("events were not independently deduplicated")
	}
}

func TestProcessEventsCursorUsesNativeIntBounds(t *testing.T) {
	record := &processRecord{events: []Event{{Sequence: 1}, {Sequence: 2}}}
	if got := record.eventsAfter("1"); len(got) != 1 || got[0].Sequence != 2 {
		t.Fatalf("cursor 1 returned %#v", got)
	}
	if got := record.eventsAfter("9223372036854775808"); len(got) != 2 {
		t.Fatalf("out-of-range cursor should be ignored, got %#v", got)
	}
}

func TestCursorFailureUsesStructuredErrorEvenWithZeroExit(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"system","subtype":"init","session_id":"cursor-fixture"}' '{"type":"result","is_error":true,"error":"approval denied"}'`)
	a := NewCursor()
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Success || got.Result.State != StateFailed {
		t.Fatalf("unexpected failure result: %#v", got.Result)
	}
	if got.Result.Error != "approval denied" {
		t.Fatalf("error = %q", got.Result.Error)
	}
}

func TestCursorFailureRetainsBoundedRedactedStderrDiagnostic(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' 'Model grok-4.6 is temporarily unavailable: workspace trust required token=top-secret' >&2; exit 17`)
	got, err := NewCursor().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.State != StateFailed || !strings.Contains(got.Result.Error, "Model grok-4.6 is temporarily unavailable") {
		t.Fatalf("stderr diagnostic not retained: %#v", got.Result)
	}
	if strings.Contains(got.Result.Error, "top-secret") {
		t.Fatalf("secret-like assignment was not redacted: %#v", got.Result)
	}
}

func TestCursorProbeClassifiesLockedMacOSKeychain(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' 'Error: Your macOS login keychain is locked.' >&2; exit 1`)
	_, err := NewCursor().Probe(context.Background(), ProbeRequest{Executable: path})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) {
		t.Fatalf("probe error=%v, want AdapterError", err)
	}
	if adapterErr.Code != ErrAuthenticationRequired || adapterErr.Details["diagnostic_code"] != "macos_login_keychain_locked" || adapterErr.Details["attention_kind"] != "authentication" {
		t.Fatalf("unexpected keychain classification: %#v", adapterErr)
	}
	if strings.Contains(fmt.Sprint(adapterErr.Details), "login keychain is locked") {
		t.Fatalf("raw native output leaked into details: %#v", adapterErr.Details)
	}
}

func TestCursorReducerFallsBackToLastAssistantMessageForEmptySuccessResult(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}]}}' '{"type":"tool_call","message":{"role":"assistant","content":[{"type":"text","text":"tool secret"}]}}' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"final answer"}]}}' '{"type":"result","subtype":"success","is_error":false}'`)
	got, err := NewCursor().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.State != StateCompleted || got.Result.Content != "final answer" {
		t.Fatalf("unexpected Cursor fallback result: %#v", got.Result)
	}
	if got.Result.Data["diagnostic_code"] != "empty_terminal_result" || got.Result.Data["result_content_source"] != "assistant_message_fallback" {
		t.Fatalf("missing Cursor fallback diagnostics: %#v", got.Result.Data)
	}
	if strings.Contains(got.Result.Content, "tool secret") {
		t.Fatalf("tool content leaked into Cursor fallback: %#v", got.Result)
	}
}

func TestCursorTerminalResultCarriesContentProvenance(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"result","result":"direct answer","is_error":false}'`)
	got, err := NewCursor().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Content != "direct answer" || got.Result.Data["result_content_source"] != "assistant_terminal_result" {
		t.Fatalf("terminal result provenance=%#v", got.Result)
	}
}

func TestCursorProgressPhasesAreMetadataOnly(t *testing.T) {
	parser := cursorParser{}
	for input, want := range map[string]string{
		`{"type":"system","subtype":"init"}`: "initializing",
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}`: "assistant",
		`{"type":"tool_call","tool":"shell","output":"secret"}`:                                           "tool",
		`{"type":"result","result":"done"}`:                                                               "completing",
	} {
		obs := parser.Parse([]byte(input), false)
		if obs.Data["progress_phase"] != want {
			t.Fatalf("input %s phase=%v want=%s", input, obs.Data["progress_phase"], want)
		}
		if _, ok := obs.Data["output"]; ok {
			t.Fatalf("tool output leaked into progress metadata: %#v", obs.Data)
		}
	}
}

func TestCodexReducerCarriesLastAgentMessageIntoTerminalResult(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"thread.started","thread_id":"thread-fixture"}' '{"type":"item.completed","item":{"id":"item-1","type":"agent_message","text":"first"}}' '{"type":"item.completed","item":{"id":"item-2","type":"agent_message","text":"final answer"}}' '{"type":"turn.completed"}'`)
	got, err := NewCodex().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.Content != "final answer" || got.Result.ContentType != "text/plain" {
		t.Fatalf("unexpected Codex result: %#v", got.Result)
	}
}

func TestCursorWorkspaceTrustTextBecomesTypedAttention(t *testing.T) {
	obs := (cursorParser{}).Parse([]byte("Workspace trust is required before continuing"), true)
	if obs.Kind != "attention" || obs.State != StateAttention || obs.Data["diagnostic_code"] != "workspace_trust_required" || obs.Data["attention_kind"] != "permission" {
		t.Fatalf("unexpected attention observation: %#v", obs)
	}
}

func TestStructuredPermissionAttentionIsTyped(t *testing.T) {
	obs := (genericParser{}).Parse([]byte(`{"type":"permission.required","message":"secret prompt"}`), false)
	if obs.Kind != "attention" || obs.State != StateAttention || obs.Data["attention_kind"] != "permission" || obs.Data["diagnostic_code"] != "permission_required" {
		t.Fatalf("unexpected attention observation: %#v", obs)
	}
	if _, present := obs.Data["message"]; present {
		t.Fatalf("attention message leaked into safe data: %#v", obs.Data)
	}
}

func TestGenericToolResultIsNotTerminalOrRetainedAsFinalContent(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"tool_output","result":"tool secret"}' '{"type":"result","status":"completed","result":"final answer"}'`)
	got, err := NewGenericProcess().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.Content != "final answer" {
		t.Fatalf("generic result retained nonterminal tool output: %#v", got.Result)
	}
	obs := (genericParser{}).Parse([]byte(`{"type":"tool_output","result":"tool secret"}`), false)
	if obs.Terminal || obs.Content != "" || obs.Summary != "" {
		t.Fatalf("tool output classified as final: %#v", obs)
	}
}

func TestOMPLiveJSONAgentEndIsTerminalSuccess(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"session","version":3,"id":"omp-fixture"}' '{"type":"agent_start"}' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"fallback answer"}],"stopReason":"stop"}}' '{"type":"turn_end"}' '{"type":"agent_end","messages":[{"role":"user","content":[{"type":"text","text":"private prompt"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"private reasoning"},{"type":"text","text":"terminal answer"}],"stopReason":"stop"}]}'`)
	a := NewOMP()
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.State != StateCompleted {
		t.Fatalf("OMP agent_end result = %#v", got.Result)
	}
	if got.Result.Content != "terminal answer" || got.Result.Data["result_content_source"] != "assistant_terminal_result" {
		t.Fatalf("OMP terminal content = %#v", got.Result)
	}
	if strings.Contains(got.Result.Content, "private") || strings.Contains(got.Result.Content, "fallback") {
		t.Fatalf("OMP retained ineligible or superseded content: %#v", got.Result)
	}
	events, err := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
	if err != nil {
		t.Fatal(err)
	}
	terminal := events[len(events)-1]
	if terminal.Kind != "terminal" || terminal.State != StateCompleted || terminal.SourceState != "agent_end" {
		t.Fatalf("OMP terminal event = %#v", terminal)
	}
}

func TestOMPAgentEndFallsBackToLastAuthoritativeAssistantMessage(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"intermediate tool narration"}],"stopReason":"toolUse"}}' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}],"stopReason":"stop"}}' '{"type":"message_end","message":{"role":"toolResult","content":[{"type":"text","text":"tool secret"}]}}' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"final answer"}],"stopReason":"stop"}}' '{"type":"agent_end","messages":[]}'`)
	got, err := NewOMP().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.Content != "final answer" {
		t.Fatalf("OMP fallback result = %#v", got.Result)
	}
	if got.Result.Data["diagnostic_code"] != "empty_terminal_result" || got.Result.Data["result_content_source"] != "assistant_message_fallback" {
		t.Fatalf("OMP fallback provenance = %#v", got.Result.Data)
	}
	if strings.Contains(got.Result.Content, "tool secret") || strings.Contains(got.Result.Content, "intermediate") {
		t.Fatalf("OMP tool content entered result: %#v", got.Result)
	}
}

func TestOMPTerminalAssistantStopReasonControlsFailureState(t *testing.T) {
	for _, test := range []struct {
		stopReason string
		wantState  State
	}{
		{stopReason: "error", wantState: StateFailed},
		{stopReason: "aborted", wantState: StateCancelled},
	} {
		line := fmt.Sprintf(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"partial answer"}],"stopReason":%q,"errorMessage":"native stopped"}]}`, test.stopReason)
		obs := (ompParser{}).Parse([]byte(line), false)
		if !obs.Terminal || obs.Success || obs.State != test.wantState || obs.Error != "native stopped" {
			t.Fatalf("stopReason=%s observation=%#v", test.stopReason, obs)
		}
	}
}

func TestOMPAssistantTextIsBoundedUTF8AndTextOnly(t *testing.T) {
	content := strings.Repeat("a", 1<<20) + "界"
	message := map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "private"}, map[string]any{"type": "text", "text": content}}}
	encoded, err := json.Marshal(map[string]any{"type": "agent_end", "messages": []any{message}})
	if err != nil {
		t.Fatal(err)
	}
	obs := (ompParser{}).Parse(encoded, false)
	if len(obs.Content) > 1<<20 || !obs.ContentTruncated || !strings.HasPrefix(obs.Content, "aaa") {
		t.Fatalf("OMP bounded content bytes=%d truncated=%t", len(obs.Content), obs.ContentTruncated)
	}
	if strings.Contains(obs.Content, "private") {
		t.Fatal("OMP reasoning entered result content")
	}
}

func TestOMPTerminalResultSurvivesAggregateStreamLimit(t *testing.T) {
	path := fixtureExecutable(t, `yes x | tr -d '\n' | head -c 2200000
printf '\n%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"result after noisy progress"}],"stopReason":"stop"}}' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"result after noisy progress"}],"stopReason":"stop"}]}'`)
	got, err := NewOMP().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.Content != "result after noisy progress" {
		t.Fatalf("terminal result after stream limit = %#v", got.Result)
	}
	if got.Result.Data["diagnostic_code"] != diagnosticStreamLimitExceeded {
		t.Fatalf("stream limit diagnostic = %#v", got.Result.Data)
	}
}

func TestOMPScannerAcceptsAdvertisedMaximumAssistantContent(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"'
yes a | tr -d '\n' | head -c 1048576
printf '%s\n' '"}],"stopReason":"stop"}]}'`)
	got, err := NewOMP().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || len(got.Result.Content) != 1<<20 {
		t.Fatalf("maximum result bytes=%d result=%#v", len(got.Result.Content), got.Result)
	}
	if got.Result.Data["diagnostic_code"] != nil {
		t.Fatalf("maximum result unexpectedly diagnosed: %#v", got.Result.Data)
	}
}

func TestOversizedStructuredRecordReturnsTypedDiagnostic(t *testing.T) {
	path := fixtureExecutable(t, `yes x | tr -d '\n' | head -c 8400000`)
	got, err := NewOMP().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Success || got.Result.Data["diagnostic_code"] != "result_extraction_failed" || got.Result.Data["parse_diagnostic_code"] != diagnosticRecordTooLarge {
		t.Fatalf("oversized record result = %#v", got.Result)
	}
}

func TestStructuredErrorFieldFailsWithoutSeparateErrorFlag(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"result","error":"native failure"}'`)
	got, err := NewGenericProcess().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Success || got.Result.State != StateFailed || got.Result.Error != "native failure" {
		t.Fatalf("structured error result = %#v", got.Result)
	}
}

func TestMalformedOutputIsHealthEventAndDoesNotBecomeSuccess(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{truncated' '{"type":"progress","session_id":"malformed-fixture"}' '{truncated-again' '{"type":"progress","session_id":"malformed-fixture"}' '{truncated-once-more'`)
	a := NewGenericProcess()
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Success || got.Result.State != StateOrphaned {
		t.Fatalf("zero-domain-result interpreted as success: %#v", got.Result)
	}
	events, err := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Kind != "health" {
		t.Fatalf("malformed output was not observable: %#v", events)
	}
}

func TestGenericResultContractCanUseBoundedResultPath(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.json")
	path := fixtureExecutable(t, `printf '%s' '{"success":true,"summary":"result-file summary"}' > "$1"`)
	got, err := NewGenericProcess().Launch(context.Background(), LaunchRequest{Argv: []string{path, resultPath}, ResultPath: resultPath, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.State != StateCompleted {
		t.Fatalf("result-file outcome = %#v", got.Result)
	}
}

func TestProbeReportsDependencyFailureWithoutMutatingStats(t *testing.T) {
	path := fixtureExecutable(t, `if [ "${1:-}" = "--version" ]; then exit 7; fi`)
	a := NewOMP()
	_, err := a.Probe(context.Background(), ProbeRequest{Executable: path, Timeout: time.Second})
	if err == nil {
		t.Fatal("expected failed version probe")
	}
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrDependencyUnavailable {
		t.Fatalf("probe error = %#v", err)
	}
	if len(a.Manifest().KnownFailures) == 0 || a.Manifest().KnownFailures[0].Classifier != "stats_probe" {
		t.Fatal("OMP manifest did not document non-read-only stats")
	}
}

func TestGenericProbeDoesNotExecuteArbitraryExecutable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	path := fixtureExecutable(t, `touch "`+marker+`"; printf '%s\n' 'would-be-version'`)
	result, err := NewGenericProcess().Probe(context.Background(), ProbeRequest{Executable: path, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result.BackendVersion != "" {
		t.Fatalf("generic probe invented backend version %q", result.BackendVersion)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("generic probe executed arbitrary executable")
	}
}

func TestNativeLaunchClassifiesCancellationBeforeChildAcceptance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := NewGenericProcess().Launch(ctx, LaunchRequest{Argv: []string{"/bin/sh", "-c", "exit 0"}, StartOnly: true})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrExecutionCancelled {
		t.Fatalf("launch error=%#v result=%#v", err, result)
	}
	if !result.Session.Ref.Empty() {
		t.Fatalf("cancelled pre-acceptance launch returned a session: %#v", result.Session)
	}
}

func TestMulticaProbeVerifiesBoundedWorkspaceEventGrammar(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args")
	path := fixtureExecutable(t, `
if [ "${1:-}" = "--version" ]; then printf '%s\n' 'multica 0.4.17'; exit 0; fi
printf '%s\n' "$*" > "`+argsPath+`"
printf '%s\n' '{"events":[],"next_cursor":"0","has_more":false}'
`)
	a := NewMultica(MulticaConfig{Binary: path, Profile: "desktop", Endpoint: "https://multica.example.test", Workspace: "workspace-test"})
	result, err := a.Probe(context.Background(), ProbeRequest{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(argv)
	for _, required := range []string{"--profile desktop", "--workspace-id workspace-test", "--server-url https://multica.example.test", "event list", "--cursor 0", "--limit 1"} {
		if !strings.Contains(got, required) {
			t.Fatalf("probe argv %q missing %q", got, required)
		}
	}
	for _, capability := range result.Capabilities {
		if capability.Name == CapabilityEvents && (capability.Status != CapabilitySupported || capability.Source != "live_probe") {
			t.Fatalf("event capability was not verified: %#v", capability)
		}
	}
}

func TestMulticaExplicitDefaultProfileNeverInventsNamedProfile(t *testing.T) {
	argv := MulticaArgv(MulticaConfig{Binary: "/bin/multica", Profile: MulticaDefaultProfile, Workspace: "workspace-test", Endpoint: "https://multica.example.test"}, "event list", "--cursor", "0", "--limit", "1")
	got := strings.Join(argv, " ")
	if strings.Contains(got, "--profile") {
		t.Fatalf("explicit native default emitted a named profile: %q", got)
	}
	for _, required := range []string{"--workspace-id workspace-test", "--server-url https://multica.example.test", "event list", "--cursor 0", "--limit 1"} {
		if !strings.Contains(got, required) {
			t.Fatalf("default-profile argv %q missing %q", got, required)
		}
	}
}

func TestOneShotWaitsForCompletionBeyondDiscoveryWindow(t *testing.T) {
	path := fixtureExecutable(t, `sleep 1; printf '%s\n' '{"type":"result","result":"status complete"}'`)
	a := newNativeAdapter(nativeConfig{Manifest: ompManifest(), Binary: path, Parser: ompParser{}, LaunchKind: "omp_status"})
	ref := SourceRef{Adapter: "omp", Kind: "omp_status", OpaqueID: "status-fixture"}
	started := time.Now()
	result, err := a.runOneShot(context.Background(), []string{path}, ref)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 750*time.Millisecond {
		t.Fatalf("one-shot returned before child completion: %s", elapsed)
	}
	if result.Result == nil || !result.Result.Success {
		t.Fatalf("one-shot result = %#v", result.Result)
	}
}

func TestCallerCancellationKillsAndReapsChild(t *testing.T) {
	path := fixtureExecutable(t, `sleep 10`)
	a := NewGenericProcess()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := a.Launch(ctx, LaunchRequest{Argv: []string{path}, DiscoveryWindow: 10 * time.Millisecond})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || (adapterErr.Code != ErrExecutionCancelled && adapterErr.Code != ErrTimeout) {
		t.Fatalf("cancellation error = %#v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancel/reap took too long: %s", elapsed)
	}
	if result.Result == nil || result.Result.State != StateCancelled {
		t.Fatalf("cancelled result = %#v", result.Result)
	}
}

func TestExplicitCancelEscalatesAfterGrace(t *testing.T) {
	path := fixtureExecutable(t, `trap '' TERM; : > "$1"; while :; do :; done`)
	ready := filepath.Join(t.TempDir(), "ready")
	a := NewGenericProcess()
	launch, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path, ready}, DiscoveryWindow: 10 * time.Millisecond, StartOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("signal fixture did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	if err := a.Cancel(context.Background(), CancelRequest{Ref: launch.Session.Ref, Signal: "term", Grace: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 75*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("cancel escalation elapsed=%s", elapsed)
	}
}

func TestStartOnlyDeadlineCancelsAndReapsChild(t *testing.T) {
	path := fixtureExecutable(t, `sleep 10`)
	a := NewGenericProcess()
	launch, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path}, Timeout: 100 * time.Millisecond, DiscoveryWindow: 10 * time.Millisecond, StartOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := a.Wait(waitCtx, launch.Session.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateCancelled {
		t.Fatalf("deadline result = %#v", result)
	}
}

func TestBuiltInManifestsHaveSchemaRequiredShapes(t *testing.T) {
	for _, a := range []Adapter{NewGenericProcess(), NewCodex(), NewCursor(), NewClaudeCode(), NewOMP(), NewMultica(MulticaConfig{Profile: "p", Workspace: "w", Issue: "i", Run: "r"})} {
		manifest := a.Manifest()
		encoded, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"schema_version", "adapter", "adapter_version", "core_compatibility", "document_schema_versions", "executable_discovery", "backend_compatibility", "capabilities", "probe_actions", "sensitivity"} {
			if _, ok := object[key]; !ok {
				t.Fatalf("%s manifest omitted schema-required %s", manifest.Adapter, key)
			}
		}
		if manifest.Adapter == "omp" {
			if _, ok := object["known_failures"].([]any); !ok {
				t.Fatalf("OMP known_failures is not schema array: %#v", object["known_failures"])
			}
			if manifest.AdapterVersion != "0.2.0" {
				t.Fatalf("OMP adapter version=%s", manifest.AdapterVersion)
			}
			for _, declaration := range manifest.Capabilities {
				if declaration.Name == CapabilityResultContent && (declaration.Implementation != CapabilitySupported || declaration.Constraints["required_output_mode"] != "json") {
					t.Fatalf("OMP result content declaration=%#v", declaration)
				}
			}
		}
		for _, declaration := range manifest.Capabilities {
			if declaration.Constraints == nil {
				t.Fatalf("%s capability %s omitted constraints", manifest.Adapter, declaration.Name)
			}
		}
	}
}

func TestResultContentNegotiationUsesExactInvocation(t *testing.T) {
	tests := []struct {
		name                      string
		manifest                  Manifest
		good, bad, afterDelimiter []string
	}{
		{name: "codex", manifest: codexManifest(), good: []string{"codex", "exec", "--json", "review"}, bad: []string{"codex", "exec", "review"}, afterDelimiter: []string{"--json"}},
		{name: "cursor", manifest: cursorManifest(), good: []string{"cursor-agent", "--print", "--output-format", "stream-json", "review"}, bad: []string{"cursor-agent", "--print", "review"}, afterDelimiter: []string{"--output-format", "stream-json"}},
		{name: "claude", manifest: claudeManifest(), good: []string{"claude", "--output-format=stream-json", "review"}, bad: []string{"claude", "review"}, afterDelimiter: []string{"--output-format=stream-json"}},
		{name: "omp", manifest: ompManifest(), good: []string{"omp", "-p", "--mode=json", "review"}, bad: []string{"omp", "-p", "review"}, afterDelimiter: []string{"--mode=json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			good := NegotiateInvocation(tt.manifest, tt.good, CapabilityResultContent)
			if good.Status == CapabilityUnavailable || good.Constraints["invocation_satisfied"] != true {
				t.Fatalf("good invocation=%#v", good)
			}
			bad := NegotiateInvocation(tt.manifest, tt.bad, CapabilityResultContent)
			if bad.Status != CapabilityUnavailable || bad.Constraints["invocation_satisfied"] != false || bad.Reason == "" {
				t.Fatalf("bad invocation=%#v", bad)
			}
		})
	}
	for _, tt := range tests {
		requirement, _ := NegotiateInvocation(tt.manifest, tt.good, CapabilityResultContent).Constraints["required_argv"].(map[string]any)
		if requirement["kind"] != "value" {
			continue
		}
		conflicting := append(append([]string(nil), tt.good...), fmt.Sprint(requirement["flag"]), "text")
		negotiated := NegotiateInvocation(tt.manifest, conflicting, CapabilityResultContent)
		if negotiated.Status != CapabilityUnavailable {
			t.Fatalf("%s accepted an earlier structured mode over a later conflict: %#v", tt.name, negotiated)
		}
	}
	for _, tt := range tests {
		argv := append(append(append([]string(nil), tt.bad...), "--"), tt.afterDelimiter...)
		negotiated := NegotiateInvocation(tt.manifest, argv, CapabilityResultContent)
		if negotiated.Status != CapabilityUnavailable {
			t.Fatalf("%s accepted structured-output text after delimiter: %#v", tt.name, negotiated)
		}
	}
}

func TestNativeManifestDoesNotOverclaimRestartRecovery(t *testing.T) {
	for _, a := range []Adapter{NewCodex(), NewCursor(), NewClaudeCode()} {
		for _, declaration := range a.Manifest().Capabilities {
			switch declaration.Name {
			case CapabilityAttach, CapabilitySnapshot, CapabilityEvents, CapabilityResult, CapabilityResultContent, CapabilityCancel:
				if declaration.Constraints["scope"] != "same_process_only" || declaration.Constraints["cross_restart"] != false {
					t.Fatalf("%s %s overclaims restart recovery: %#v", a.Name(), declaration.Name, declaration.Constraints)
				}
			}
		}
	}
}

func TestBoundedPollFallbackUsesObservationOrdering(t *testing.T) {
	path := fixtureExecutable(t, `if [ "${2:-}" = "status" ]; then printf '%s\n' '{"status":"running","run_id":"poll-fixture"}'; else printf '%s\n' '{"status":"completed","run_id":"poll-fixture"}'; fi`)
	config := nativeConfig{Manifest: ompManifest(), Binary: path, Parser: ompParser{}, PollStatus: true, LaunchKind: "omp_session", StatusArgs: func(ref SourceRef) []string { return []string{path, "status", "--session", ref.OpaqueID} }}
	a := newNativeAdapter(config)
	ref := SourceRef{Adapter: "omp", Kind: "omp_session", OpaqueID: "poll-fixture"}
	events, err := a.Events(context.Background(), EventsRequest{Ref: ref, Poll: PollOptions{Attempts: 1, Interval: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Ordering != "observation" || events[0].Payload["source"] != "bounded_poll" {
		t.Fatalf("fallback event = %#v", events)
	}
}

func TestMulticaArgvBindsAllAuthoritySelectors(t *testing.T) {
	config := MulticaConfig{Binary: "/tmp/multica", Profile: "profile-fixture", Workspace: "workspace-fixture", Issue: "issue-fixture", Run: "run-fixture"}
	argv := MulticaArgv(config, "event list", "--output", "json")
	expected := []string{"/tmp/multica", "--profile", "profile-fixture", "--workspace-id", "workspace-fixture", "event", "list", "--output", "json"}
	if strings.Join(argv, "\x00") != strings.Join(expected, "\x00") {
		t.Fatalf("argv = %#v", argv)
	}
	if got := MulticaArgv(config, "status"); got != nil {
		t.Fatalf("unverified status command was built: %#v", got)
	}
	watch := MulticaArgv(config, "event watch", "--cursor", "evt1.abc", "--limit", "100", "--interval", "15s")
	if strings.Join(watch, "\x00") != strings.Join([]string{"/tmp/multica", "--profile", "profile-fixture", "--workspace-id", "workspace-fixture", "event", "watch", "--cursor", "evt1.abc", "--limit", "100", "--interval", "15s"}, "\x00") {
		t.Fatalf("watch argv = %#v", watch)
	}
}

func TestMulticaBoundedEventPageFiltersAndAdvancesCursor(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "argv")
	path := fixtureExecutable(t, `printf '%s\n' "$@" > "`+argvPath+`"; printf '%s\n' '{"events":[{"type":"issue.progress","issue_id":"unrelated-issue","run_id":"unrelated-run","sequence":4},{"type":"run.completed","run_id":"run-fixture","sequence":7,"state":"completed"}],"next_cursor":"evt-next"}'`)
	config := MulticaConfig{Binary: path, Profile: "profile-fixture", Workspace: "workspace-fixture", Issue: "issue-fixture", Run: "run-fixture", EventPageLimit: 42}
	adapter := NewMultica(config)
	paged, ok := adapter.(PagedEvents)
	if !ok {
		t.Fatal("Multica adapter does not expose bounded event pages")
	}
	page, err := paged.EventsPage(context.Background(), EventsRequest{Ref: SourceRef{Adapter: "multica", Kind: "multica_event", Profile: config.Profile, Workspace: config.Workspace}, Cursor: "evt-prev"})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "evt-next" || page.Scanned != 2 || page.Filtered != 1 || len(page.Events) != 1 {
		t.Fatalf("page = %#v", page)
	}
	if page.Events[0].State != StateCompleted || page.Events[0].Payload["run_id"] != config.Run {
		t.Fatalf("terminal event = %#v", page.Events[0])
	}
	argvBytes, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Fields(string(argvBytes))
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "event watch") || strings.Contains(joined, "watch") {
		t.Fatalf("bounded page invoked watch: %q", joined)
	}
	for _, expected := range []string{"--profile", config.Profile, "--workspace-id", config.Workspace, "event", "list", "--cursor", "evt-prev", "--limit", "42"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("argv %q missing %q", joined, expected)
		}
	}
	filteredAdapter := NewMultica(MulticaConfig{Binary: path, Profile: config.Profile, Workspace: config.Workspace, Issue: "missing-issue", Run: "missing-run", EventPageLimit: 42})
	observed, err := filteredAdapter.Events(context.Background(), EventsRequest{Ref: SourceRef{Adapter: "multica", Kind: "multica_event", Profile: config.Profile, Workspace: config.Workspace}, Cursor: "evt-prev"})
	if err != nil || len(observed) != 1 || observed[0].Kind != "health" || observed[0].Cursor != "evt-next" {
		t.Fatalf("cursor-only observation = %#v, err=%v", observed, err)
	}
}

func TestMulticaIssueBindingMatchesNestedTaskAndUsesAuthorityCursors(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"events":[{"type":"issue.created","aggregate_kind":"issue","aggregate_id":"issue-fixture","sequence":101,"occurred_at":"2026-08-10T00:00:01Z"},{"type":"task.updated","aggregate_kind":"task","aggregate_id":"task-fixture","sequence":102,"payload":{"issue_id":"issue-fixture","run_id":"run-fixture","status":"completed"}},{"type":"issue.created","aggregate_kind":"issue","aggregate_id":"other-issue","sequence":103}],"next_cursor":"evt-next"}'`)
	config := MulticaConfig{Binary: path, Profile: "profile-fixture", Workspace: "workspace-fixture", Issue: "issue-fixture", EventPageLimit: 3}
	adapter := NewMultica(config)
	paged, ok := adapter.(PagedEvents)
	if !ok {
		t.Fatal("Multica adapter does not expose bounded event pages")
	}
	ref := SourceRef{Adapter: "multica", Kind: "multica_event", Profile: config.Profile, Workspace: config.Workspace}
	page, err := paged.EventsPage(context.Background(), EventsRequest{Ref: ref, Cursor: "evt-prev"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Scanned != 3 || page.Filtered != 1 || len(page.Events) != 2 || page.NextCursor != "evt-next" {
		t.Fatalf("issue-scoped page = %#v", page)
	}
	if page.Events[0].Cursor != "101" || page.Events[1].Cursor != "102" || page.Events[0].OccurredAt == nil {
		t.Fatalf("events did not retain authority cursors: %#v", page.Events)
	}
	if page.Events[1].Kind != "progress" || page.Events[1].State != StateWaiting || page.Events[1].Payload["issue_id"] != config.Issue || page.Events[1].Payload["run_id"] != "run-fixture" {
		t.Fatalf("nested task event was not correlated: %#v", page.Events[1])
	}
	observed, err := adapter.Events(context.Background(), EventsRequest{Ref: ref, Cursor: "evt-prev"})
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 3 {
		t.Fatalf("expected matching events plus page checkpoint, got %#v", observed)
	}
	checkpoint := observed[len(observed)-1]
	if checkpoint.Kind != "health" || checkpoint.Cursor != "evt-next" || checkpoint.Payload["page_checkpoint"] != true {
		t.Fatalf("missing page checkpoint: %#v", checkpoint)
	}
}

func TestMulticaEventPageAcceptsPrettyPrintedJSON(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{' '  "events": [' '    {' '      "type": "issue:updated",' '      "aggregate_kind": "issue",' '      "aggregate_id": "issue-fixture",' '      "sequence": 2,' '      "payload": {"status": "done"}' '    }' '  ],' '  "next_cursor": "2",' '  "has_more": false' '}'`)
	config := MulticaConfig{Binary: path, Profile: "profile-fixture", Workspace: "workspace-fixture", Issue: "issue-fixture"}
	paged := NewMultica(config).(PagedEvents)
	page, err := paged.EventsPage(context.Background(), EventsRequest{Ref: SourceRef{Adapter: "multica", Kind: "multica_issue", Profile: config.Profile, Workspace: config.Workspace, Issue: config.Issue}, Cursor: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "2" || page.Scanned != 1 || len(page.Events) != 1 {
		t.Fatalf("pretty page = %#v", page)
	}
	event := page.Events[0]
	if event.Kind != "terminal" || event.State != StateCompleted || event.Payload["aggregate_id"] != config.Issue {
		t.Fatalf("pretty terminal event = %#v", event)
	}
}

func TestRequiredContextFailsBeforeChildLaunch(t *testing.T) {
	called := filepath.Join(t.TempDir(), "called")
	path := fixtureExecutable(t, `touch "$1"`)
	a := NewGenericProcess()
	_, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path, called}, Context: &ContextInput{Path: "/private/context", Required: true}})
	if err == nil {
		t.Fatal("expected context capability failure")
	}
	if _, statErr := os.Stat(called); !os.IsNotExist(statErr) {
		t.Fatal("child was launched despite required context failure")
	}
}
