package adapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeParserReadsCompletedTextPart(t *testing.T) {
	obs := (opencodeJSONParser{}).Parse([]byte(`{"type":"text","sessionID":"ses_fixture","part":{"type":"text","text":"answer one","time":{"start":1,"end":2}}}`), false)
	if obs.Terminal || obs.Success {
		t.Fatalf("text part classified terminal: %#v", obs)
	}
	if obs.SessionID != "ses_fixture" || obs.Content != "answer one" || obs.ContentType != "text/plain" || obs.SourceState != "opencode.text" {
		t.Fatalf("obs = %#v", obs)
	}
	if obs.Kind == "" || obs.Data["completed"] != true {
		t.Fatalf("missing metadata event: %#v", obs)
	}
	if _, leaked := obs.Data["text"]; leaked {
		t.Fatalf("assistant text leaked into event metadata: %#v", obs.Data)
	}
}

func TestOpenCodeParserIgnoresInFlightTextAndNonTextParts(t *testing.T) {
	for _, line := range []string{
		`{"type":"text","sessionID":"ses_fixture","part":{"type":"text","text":"draft delta","time":{"start":1}}}`,
		`{"type":"text","sessionID":"ses_fixture","part":{"type":"text","text":"   ","time":{"start":1,"end":2}}}`,
		`{"type":"reasoning","sessionID":"ses_fixture","part":{"type":"reasoning","text":"private chain","time":{"end":2}}}`,
	} {
		obs := (opencodeJSONParser{}).Parse([]byte(line), false)
		if obs.Terminal || obs.Content != "" {
			t.Fatalf("line %s produced content: %#v", line, obs)
		}
		if obs.Kind == "" {
			t.Fatalf("line %s produced no metadata event: %#v", line, obs)
		}
	}
}

func TestOpenCodeParserNeverPersistsToolPayloads(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"ses_fixture","part":{"type":"tool","tool":"bash","input":{"command":"cat secret-file"},"output":"tool-secret-output"}}`
	obs := (opencodeJSONParser{}).Parse([]byte(line), false)
	if obs.Content != "" || obs.Terminal || obs.Kind == "" {
		t.Fatalf("tool_use obs = %#v", obs)
	}
	raw, err := json.Marshal(obs.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-file", "tool-secret-output", "cat "} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("tool payload leaked into metadata: %s", raw)
		}
	}
}

func TestOpenCodeParserStepFinishIsNeverTerminal(t *testing.T) {
	for _, reason := range []string{"tool-calls", "stop"} {
		obs := (opencodeJSONParser{}).Parse([]byte(`{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"`+reason+`"}}`), false)
		if obs.Terminal || obs.Success || obs.Error != "" {
			t.Fatalf("step_finish %s obs = %#v", reason, obs)
		}
		if obs.Kind == "" || obs.Data["reason"] != reason {
			t.Fatalf("step_finish %s metadata = %#v", reason, obs.Data)
		}
	}
}

func TestOpenCodeParserExtractsNestedErrorMessage(t *testing.T) {
	obs := (opencodeJSONParser{}).Parse([]byte(`{"type":"error","sessionID":"s","error":{"name":"ProviderError","data":{"message":"upstream exploded"}}}`), false)
	if obs.Terminal || obs.Error != "upstream exploded" || obs.State != StateFailed || obs.Kind == "" {
		t.Fatalf("error obs = %#v", obs)
	}
	plain := (opencodeJSONParser{}).Parse([]byte(`{"type":"error","error":"plain failure"}`), false)
	if plain.Error != "plain failure" {
		t.Fatalf("string error obs = %#v", plain)
	}
}

func TestOpenCodeParserMalformedAndStderrLines(t *testing.T) {
	malformed := (opencodeJSONParser{}).Parse([]byte("not json at all"), false)
	if malformed.Terminal || malformed.Content != "" || malformed.Kind == "" || malformed.SourceState != "malformed_output" {
		t.Fatalf("malformed obs = %#v", malformed)
	}
	stderr := (opencodeJSONParser{}).Parse([]byte(`{"type":"text","part":{"type":"text","text":"stderr answer","time":{"end":1}}}`), true)
	if stderr.Content != "" || stderr.SourceState != "stderr" {
		t.Fatalf("stderr obs = %#v", stderr)
	}
}

const opencodeStreamFixture = `printf '%s\n' \
'{"type":"step_start","sessionID":"ses_fixture","part":{"type":"step-start"}}' \
'{"type":"tool_use","sessionID":"ses_fixture","part":{"type":"tool","tool":"bash","input":{"command":"cat secret-file"},"output":"tool-secret-output"}}' \
'{"type":"reasoning","sessionID":"ses_fixture","part":{"type":"reasoning","text":"reasoning-secret"}}' \
'{"type":"text","sessionID":"ses_fixture","part":{"type":"text","text":"pretool narration","time":{"start":1,"end":2}}}' \
'{"type":"step_finish","sessionID":"ses_fixture","part":{"type":"step-finish","reason":"tool-calls"}}' \
'{"type":"tool_use","sessionID":"ses_fixture","part":{"type":"tool","tool":"read","input":{"path":"secret-path"},"output":"more-tool-secret"}}' \
'{"type":"text","sessionID":"ses_fixture","part":{"type":"text","text":"first answer","time":{"start":3,"end":4}}}' \
'{"type":"text","sessionID":"ses_fixture","part":{"type":"text","text":"second answer","time":{"start":5,"end":6}}}' \
'{"type":"step_finish","sessionID":"ses_fixture","part":{"type":"step-finish","reason":"stop"}}'`

func TestOpenCodeRunAssemblesOrderedTextParts(t *testing.T) {
	path := fixtureExecutable(t, opencodeStreamFixture)
	a := NewOpenCode()
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path, "run", "--format", "json"}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.State != StateCompleted {
		t.Fatalf("result = %#v", got.Result)
	}
	if got.Result.Content != "first answer\nsecond answer" || got.Result.ContentType != "text/plain" {
		t.Fatalf("content = %#v", got.Result.Content)
	}
	if strings.Contains(got.Result.Content, "pretool narration") {
		t.Fatalf("pre-tool-calls step text entered result: %q", got.Result.Content)
	}
	if got.Result.Data["result_content_source"] != "assistant_terminal_result" || got.Result.Data["terminal_source_state"] != "opencode.run" {
		t.Fatalf("provenance = %#v", got.Result.Data)
	}
	if got.Session.Ref.OpaqueID != "ses_fixture" {
		t.Fatalf("session ref = %#v", got.Session.Ref)
	}
	if strings.Contains(got.Result.Content, "secret") || strings.Contains(got.Result.Content, "reasoning") {
		t.Fatalf("tool or reasoning payload entered result: %q", got.Result.Content)
	}
	events, err := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no metadata events recorded")
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-file", "tool-secret-output", "more-tool-secret", "reasoning-secret", "pretool narration", "first answer", "second answer"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("payload leaked into events: %s", raw)
		}
	}
	sawText := false
	for _, event := range events {
		if event.SourceState == "opencode.text" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("missing text metadata event: %#v", events)
	}
	result, err := a.Result(context.Background(), ResultRequest{Ref: got.Session.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Content != got.Result.Content {
		t.Fatalf("same-process result = %#v", result)
	}
}

func TestOpenCodeStepFinishResetsPartsAfterStreamLimit(t *testing.T) {
	pretool := []byte(`{"type":"text","sessionID":"s","part":{"type":"text","text":"pretool narration","time":{"end":1}}}`)
	record := &processRecord{parser: opencodeJSONParser{}, maxOutput: len(pretool)}
	record.ingest(pretool, false)
	record.ingest([]byte(`{"type":"tool_use","sessionID":"s","part":{"type":"tool","tool":"bash","input":{"command":"`+strings.Repeat("x", 80)+`"},"output":"huge"}}`), false)
	record.ingest([]byte(`{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"tool-calls"}}`), false)
	record.ingest([]byte(`{"type":"text","sessionID":"s","part":{"type":"text","text":"final answer","time":{"end":2}}}`), false)
	if got := strings.Join(record.opencodeParts, "\n"); got != "final answer" {
		t.Fatalf("assembled parts after stream-limit step reset = %q", got)
	}
	if len(record.parseWarnings) == 0 || record.parseWarnings[0] != diagnosticStreamLimitExceeded {
		t.Fatalf("parse warnings = %#v", record.parseWarnings)
	}
}

func TestOpenCodeRunFailureModes(t *testing.T) {
	t.Run("nonzero exit beats collected text", func(t *testing.T) {
		path := fixtureExecutable(t, `printf '%s\n' '{"type":"text","sessionID":"s","part":{"type":"text","text":"partial","time":{"end":1}}}'; exit 3`)
		got, err := NewOpenCode().Launch(context.Background(), LaunchRequest{Argv: []string{path, "run", "--format", "json"}, DiscoveryWindow: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if got.Result == nil || got.Result.Success || got.Result.State != StateFailed {
			t.Fatalf("result = %#v", got.Result)
		}
	})
	t.Run("error event beats collected text on clean exit", func(t *testing.T) {
		path := fixtureExecutable(t, `printf '%s\n' '{"type":"text","sessionID":"s","part":{"type":"text","text":"partial","time":{"end":1}}}' '{"type":"error","sessionID":"s","error":{"name":"ProviderError","data":{"message":"upstream exploded"}}}'`)
		got, err := NewOpenCode().Launch(context.Background(), LaunchRequest{Argv: []string{path, "run", "--format", "json"}, DiscoveryWindow: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if got.Result == nil || got.Result.Success || got.Result.State != StateFailed || !strings.Contains(got.Result.Error, "upstream exploded") {
			t.Fatalf("result = %#v", got.Result)
		}
	})
	t.Run("clean exit without assistant text is empty terminal result", func(t *testing.T) {
		path := fixtureExecutable(t, `printf '%s\n' '{"type":"step_start","sessionID":"s","part":{"type":"step-start"}}' '{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"stop"}}'`)
		got, err := NewOpenCode().Launch(context.Background(), LaunchRequest{Argv: []string{path, "run", "--format", "json"}, DiscoveryWindow: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if got.Result == nil || got.Result.Success || got.Result.State != StateFailed || got.Result.Data["diagnostic_code"] != "empty_terminal_result" {
			t.Fatalf("result = %#v", got.Result)
		}
	})
	t.Run("pretool text followed only by tool-calls step is empty terminal result", func(t *testing.T) {
		path := fixtureExecutable(t, `printf '%s\n' '{"type":"step_start","sessionID":"s","part":{"type":"step-start"}}' '{"type":"text","sessionID":"s","part":{"type":"text","text":"pretool only","time":{"end":1}}}' '{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"tool-calls"}}' '{"type":"tool_use","sessionID":"s","part":{"type":"tool","tool":"bash","input":{"command":"ls"},"output":"out"}}' '{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"stop"}}'`)
		got, err := NewOpenCode().Launch(context.Background(), LaunchRequest{Argv: []string{path, "run", "--format", "json"}, DiscoveryWindow: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if got.Result == nil || got.Result.Success || got.Result.State != StateFailed || got.Result.Data["diagnostic_code"] != "empty_terminal_result" {
			t.Fatalf("result = %#v", got.Result)
		}
	})
	t.Run("error message is redacted before reaching result and events", func(t *testing.T) {
		path := fixtureExecutable(t, `printf '%s\n' '{"type":"error","sessionID":"s","error":{"name":"ProviderError","data":{"message":"upstream exploded token=private-value"}}}'`)
		a := NewOpenCode()
		got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path, "run", "--format", "json"}, DiscoveryWindow: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if got.Result == nil || got.Result.Success || got.Result.State != StateFailed {
			t.Fatalf("result = %#v", got.Result)
		}
		if !strings.Contains(got.Result.Error, "upstream exploded") || !strings.Contains(got.Result.Error, "[REDACTED]") || strings.Contains(got.Result.Error, "private-value") {
			t.Fatalf("error diagnostic not redacted: %q", got.Result.Error)
		}
		events, err := a.Events(context.Background(), EventsRequest{Ref: got.Session.Ref})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(events)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "private-value") {
			t.Fatalf("unredacted diagnostic entered events: %s", raw)
		}
	})
	t.Run("malformed stream without text is not a successful answer", func(t *testing.T) {
		path := fixtureExecutable(t, `printf '%s\n' 'unstructured banner'`)
		got, err := NewOpenCode().Launch(context.Background(), LaunchRequest{Argv: []string{path, "run", "--format", "json"}, DiscoveryWindow: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if got.Result == nil || got.Result.Success || got.Result.Content != "" {
			t.Fatalf("result = %#v", got.Result)
		}
	})
}

func TestOpenCodeCancellationRemainsCancelled(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"text","sessionID":"s","part":{"type":"text","text":"partial","time":{"end":1}}}'; sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	got, err := NewOpenCode().Launch(ctx, LaunchRequest{Argv: []string{path, "run", "--format", "json"}, DiscoveryWindow: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if got.Result == nil || got.Result.Success || got.Result.State != StateCancelled {
		t.Fatalf("cancelled result = %#v err=%v", got.Result, err)
	}
}

func TestOpenCodeResultContentRequiresExactRunInvocation(t *testing.T) {
	manifest := NewOpenCode().Manifest()
	valid := NegotiateInvocation(manifest, []string{"opencode", "run", "--format", "json", "--model", "zai/glm-5.3", "--", "task"}, CapabilityResultContent)
	if valid.Status != CapabilitySupported || valid.Constraints["invocation_satisfied"] != true {
		t.Fatalf("valid invocation = %#v", valid)
	}
	equalsForm := NegotiateInvocation(manifest, []string{"opencode", "run", "--format=json"}, CapabilityResultContent)
	if equalsForm.Status != CapabilitySupported {
		t.Fatalf("--format=json form = %#v", equalsForm)
	}
	for _, argv := range [][]string{
		{"opencode"},
		{"opencode", "--format", "json"},
		{"opencode", "agent", "run", "--format", "json"},
		{"opencode", "run"},
		{"opencode", "run", "--format", "text"},
		{"opencode", "run", "--format=json", "--format", "text"},
		{"opencode", "run", "--", "--format", "json"},
		{"opencode", "run", "task", "--format"},
	} {
		got := NegotiateInvocation(manifest, argv, CapabilityResultContent)
		if got.Status != CapabilityUnavailable || got.Constraints["invocation_satisfied"] != false {
			t.Fatalf("argv %v negotiated %#v", argv, got)
		}
	}
}

func TestOpenCodeManifestDeclaresBoundedSameProcessContract(t *testing.T) {
	m := NewOpenCode().Manifest()
	if m.Adapter != "opencode" || m.AdapterVersion != "0.1.0" {
		t.Fatalf("manifest = %#v", m)
	}
	if len(m.ExecutableDiscovery) != 1 || m.ExecutableDiscovery[0].Value != "opencode" || m.ExecutableDiscovery[0].Kind != "path_lookup" {
		t.Fatalf("discovery = %#v", m.ExecutableDiscovery)
	}
	if len(m.ProbeActions) != 1 || m.ProbeActions[0].Argv[1] != "--version" || m.ProbeActions[0].SideEffectClass != "read_only" {
		t.Fatalf("probe actions = %#v", m.ProbeActions)
	}
	status := map[CapabilityName]CapabilityDeclaration{}
	for _, decl := range m.Capabilities {
		status[decl.Name] = decl
	}
	if status[CapabilityLaunch].Implementation != CapabilitySupported || status[CapabilityResult].Implementation != CapabilitySupported || status[CapabilityResultContent].Implementation != CapabilitySupported {
		t.Fatalf("declarations = %#v", status)
	}
	for _, name := range []CapabilityName{CapabilityAttach, CapabilitySnapshot, CapabilityEvents, CapabilityContextInjection} {
		if status[name].Implementation != CapabilityConditional {
			t.Fatalf("%s = %#v", name, status[name])
		}
	}
	for _, name := range []CapabilityName{CapabilityAttach, CapabilitySnapshot, CapabilityEvents, CapabilityResult, CapabilityResultContent, CapabilityCancel} {
		if status[name].Constraints["scope"] != "same_process_only" || status[name].Constraints["cross_restart"] != false {
			t.Fatalf("%s constraints = %#v", name, status[name].Constraints)
		}
	}
	if status[CapabilityEvents].Constraints["content"] != "metadata_only" {
		t.Fatalf("events constraints = %#v", status[CapabilityEvents].Constraints)
	}
	if status[CapabilityResume].Implementation != CapabilityUnavailable {
		t.Fatalf("resume = %#v", status[CapabilityResume])
	}
	content := status[CapabilityResultContent]
	if content.Constraints["required_output_mode"] != "json" || content.Constraints["required_subcommand"] != "run" {
		t.Fatalf("result_content constraints = %#v", content.Constraints)
	}
	requiredArgv, ok := content.Constraints["required_argv"].(map[string]any)
	if !ok || requiredArgv["flag"] != "--format" || requiredArgv["kind"] != "value" || requiredArgv["value"] != "json" {
		t.Fatalf("required_argv = %#v", content.Constraints["required_argv"])
	}
	if content.Constraints["source"] != "assistant_terminal_result" {
		t.Fatalf("source = %#v", content.Constraints["source"])
	}
	for name, decl := range status {
		negotiated := NegotiateInvocation(m, nil, name)
		if negotiated.Status == CapabilityUnavailable && decl.Implementation != CapabilityUnavailable {
			t.Fatalf("%s unexpectedly unavailable", name)
		}
	}
	if got := NegotiateInvocation(m, nil, "launch-kind"); got.Reason != "capability is not declared" {
		t.Fatalf("unknown capability = %#v", got)
	}
}

func TestOpenCodeProbeUsesVersionFixture(t *testing.T) {
	path := fixtureExecutable(t, `if [ "${1:-}" = "--version" ]; then printf 'opencode 1.17.13\n'; exit 0; fi; exit 2`)
	got, err := NewOpenCode().Probe(context.Background(), ProbeRequest{Executable: path})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, capability := range got.Capabilities {
		if capability.Name == CapabilityResultContent && capability.Status == CapabilitySupported {
			found = true
		}
	}
	if !found {
		t.Fatalf("probe capabilities = %#v", got.Capabilities)
	}
}
