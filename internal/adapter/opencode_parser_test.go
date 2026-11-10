package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	if got := record.opencodeText.String(); got != "final answer" {
		t.Fatalf("assembled parts after stream-limit step reset = %q", got)
	}
	if len(record.parseWarnings) == 0 || record.parseWarnings[0] != diagnosticStreamLimitExceeded {
		t.Fatalf("parse warnings = %#v", record.parseWarnings)
	}
}

// A first event dropped for the output limit is still an event: the early-exit
// diagnosis must not claim OpenCode emitted nothing.
func TestOpenCodeEventDroppedForStreamLimitStillCounts(t *testing.T) {
	tool := []byte(`{"type":"tool_use","sessionID":"s","part":{"type":"tool","tool":"bash","input":{"command":"` + strings.Repeat("x", 80) + `"},"output":"huge"}}`)
	record := &processRecord{parser: opencodeJSONParser{}, maxOutput: 16}
	record.ingest(tool, false)
	if len(record.parseWarnings) == 0 || record.parseWarnings[0] != diagnosticStreamLimitExceeded {
		t.Fatalf("event was not over the limit: warnings = %#v", record.parseWarnings)
	}
	if !record.sawOpenCode {
		t.Fatal("an OpenCode event dropped for the stream limit was not recorded as seen")
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

func TestOpenCodeParserRecordsStructuredRetryWithoutMessage(t *testing.T) {
	line := `{"type":"retry","timestamp":1794251075486,"sessionID":"ses_retry","attempt":2,"nextRetryAt":1794251101486,"error":{"type":"provider.rate-limit","message":"Rate limit exceeded; secret details","status":429,"retryAfterMs":26000}}`
	obs := (opencodeJSONParser{}).Parse([]byte(line), false)
	if obs.SourceState != "opencode.retry" || obs.State != StateRunning || obs.Error != "" {
		t.Fatalf("retry observation = %#v", obs)
	}
	for key, want := range map[string]any{"attempt": int64(2), "retry_after_ms": int64(26000), "provider_error_type": "provider.rate-limit", "http_status": int64(429), "next_retry_at": "2026-11-09T19:05:01.486Z"} {
		if got := obs.Data[key]; got != want {
			t.Fatalf("retry %s = %#v, want %#v (all=%#v)", key, got, want, obs.Data)
		}
	}
	encoded, err := json.Marshal(obs.Data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Rate limit exceeded") || strings.Contains(string(encoded), "secret details") {
		t.Fatalf("provider message leaked into retry metadata: %s", encoded)
	}
	noHTTPProof := (opencodeJSONParser{}).Parse([]byte(`{"type":"retry","attempt":1,"error":{"type":"provider.rate-limit","code":429}}`), false)
	if _, exists := noHTTPProof.Data["http_status"]; exists {
		t.Fatalf("bare provider code counted as HTTP status: %#v", noHTTPProof.Data)
	}
	stringStatus := (opencodeJSONParser{}).Parse([]byte(`{"type":"retry","attempt":1,"error":{"status":"429"}}`), false)
	if _, exists := stringStatus.Data["http_status"]; exists {
		t.Fatalf("string status counted as explicit numeric HTTP status: %#v", stringStatus.Data)
	}
}

func TestOpenCodeAggregatesTokenUsageAcrossSteps(t *testing.T) {
	record := &processRecord{parser: opencodeJSONParser{}, done: make(chan struct{})}
	for _, line := range []string{
		`{"type":"step_start","part":{"type":"step-start"}}`,
		`{"type":"text","part":{"type":"text","text":"first","time":{"end":1}}}`,
		`{"type":"step_finish","part":{"type":"step-finish","reason":"tool-calls","tokens":{"input":10,"output":2,"reasoning":1,"cache":{"read":7,"write":3}}}}`,
		`{"type":"step_start","part":{"type":"step-start"}}`,
		`{"type":"text","part":{"type":"text","text":"final","time":{"end":2}}}`,
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"input":5,"output":4,"reasoning":0,"cache":{"read":6,"write":1}}}}`,
	} {
		record.ingest([]byte(line), false)
	}
	record.finish(nil)
	if !record.result.Success || record.result.TokenUsage == nil {
		t.Fatalf("result usage = %#v", record.result)
	}
	want := (TokenUsage{Input: 15, Output: 6, CacheRead: 13, CacheWrite: 4, Reasoning: 1})
	if *record.result.TokenUsage != want {
		t.Fatalf("usage = %#v, want %#v", *record.result.TokenUsage, want)
	}
}

func TestOpenCodeTokenUsageSaturationIsExplicit(t *testing.T) {
	usage := TokenUsage{Input: maxTokenUsageCount, Output: maxTokenUsageCount}
	usage.Add(TokenUsage{Input: 1})
	usage.Add(TokenUsage{Output: 1, Saturated: true})
	if usage.Input != maxTokenUsageCount || usage.Output != maxTokenUsageCount || !usage.Saturated {
		t.Fatalf("saturated usage = %#v", usage)
	}
}

func TestOpenCodeProgressAfterRetryClearsCapacityEvidence(t *testing.T) {
	record := &processRecord{parser: opencodeJSONParser{}, done: make(chan struct{}), ref: SourceRef{Adapter: "opencode"}}
	for _, line := range []string{
		`{"type":"step_start","part":{"type":"step-start"}}`,
		`{"type":"retry","attempt":1,"error":{"type":"provider.rate-limit","status":429}}`,
		`{"type":"step_start","part":{"type":"step-start"}}`,
		`{"type":"text","part":{"type":"text","text":"partial","time":{"end":1}}}`,
	} {
		record.ingest([]byte(line), false)
	}
	if len(record.events) != 4 || record.events[2].SourceState != "opencode.step_start" {
		t.Fatalf("duplicate OpenCode steps coalesced, events=%#v", record.events)
	}
	record.finish(errors.New("exit status 1"))
	if record.result.Data["failure_category"] == "capacity" || record.result.Data["http_status"] == int64(429) {
		t.Fatalf("later unrelated failure inherited retry evidence: %#v", record.result)
	}
}

func TestOpenCodeTerminalRateLimitFailureHasExplicitCapacityEvidence(t *testing.T) {
	record := &processRecord{parser: opencodeJSONParser{}, done: make(chan struct{}), ref: SourceRef{Adapter: "opencode"}}
	record.ingest([]byte(`{"type":"retry","attempt":2,"error":{"type":"provider.rate-limit","status":429,"retryAfterMs":26000}}`), false)
	record.ingest([]byte(`{"type":"error","error":{"type":"provider.rate-limit","message":"Rate limit exceeded","status":429}}`), false)
	record.finish(nil)
	if record.result == nil || record.result.State != StateFailed || record.result.Data["failure_category"] != "capacity" || record.result.Data["http_status"] != int64(429) {
		t.Fatalf("rate-limit result = %#v", record.result)
	}
	bareCode := &processRecord{parser: opencodeJSONParser{}, done: make(chan struct{}), ref: SourceRef{Adapter: "opencode"}}
	bareCode.ingest([]byte(`{"type":"error","error":{"type":"provider.rate-limit","message":"Rate limit exceeded","code":429}}`), false)
	bareCode.finish(nil)
	if _, exists := bareCode.result.Data["failure_category"]; exists {
		t.Fatalf("bare provider code became capacity proof: %#v", bareCode.result)
	}
}

func TestRetainObservationAfterLimitKeepsOnlyStatefulOpenCodeEvidence(t *testing.T) {
	for _, sourceState := range []string{"opencode.retry", "opencode.step_start", "opencode.step_finish"} {
		if !retainObservationAfterLimit(parsedObservation{Kind: "progress", SourceState: sourceState}, false) {
			t.Fatalf("%s dropped after the stream budget", sourceState)
		}
	}
	for _, sourceState := range []string{"opencode.reasoning", "opencode.tool_use", "opencode.text"} {
		if retainObservationAfterLimit(parsedObservation{Kind: "progress", SourceState: sourceState}, false) {
			t.Fatalf("%s bypassed the stream budget", sourceState)
		}
	}
	if !retainObservationAfterLimit(parsedObservation{Kind: "progress", SourceState: "opencode.text"}, true) {
		t.Fatal("post-retry text cannot clear stale rate-limit evidence")
	}
}

func TestOpenCodeRepeatedReasoningMetadataStaysBounded(t *testing.T) {
	record := &processRecord{parser: opencodeJSONParser{}, maxOutput: 1 << 10, done: make(chan struct{}), ref: SourceRef{Adapter: "opencode"}}
	line := []byte(`{"type":"reasoning","sessionID":"s","part":{"type":"reasoning","text":"private chain of thought"}}`)
	for i := 0; i < 50000; i++ {
		record.ingest(line, false)
	}
	if len(record.observations) != 1 || len(record.events) != 1 {
		t.Fatalf("repeatable reasoning was not coalesced: observations=%d events=%d", len(record.observations), len(record.events))
	}
	if record.observations[0].SourceState != "opencode.reasoning" || record.observations[0].Content != "" {
		t.Fatalf("bounded reasoning observation = %#v", record.observations[0])
	}
	if len(record.parseWarnings) != 1 || record.parseWarnings[0] != diagnosticStreamLimitExceeded {
		t.Fatalf("stream budget warning = %#v", record.parseWarnings)
	}
}

func TestOpenCodeStepControlsSurviveStreamLimit(t *testing.T) {
	record := &processRecord{parser: opencodeJSONParser{}, maxOutput: 64, done: make(chan struct{}), ref: SourceRef{Adapter: "opencode"}}
	retry := []byte(`{"type":"retry","attempt":1,"error":{"type":"provider.rate-limit","status":429}}`)
	finish := []byte(`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}`)
	const frames = 1000
	for i := 0; i < frames; i++ {
		record.ingest(retry, false)
		record.ingest(finish, false)
	}
	counts := map[string]int{}
	for _, event := range record.events {
		counts[event.SourceState]++
	}
	if counts["opencode.retry"] != frames || counts["opencode.step_finish"] != frames || len(record.observations) != 2*frames {
		t.Fatalf("step control evidence was coalesced or dropped: events=%#v observations=%d", counts, len(record.observations))
	}
}

func TestOpenCodeProviderErrorTypeIsAllowlisted(t *testing.T) {
	for _, raw := range []string{
		"sk-live-51H8xQ2eZ9",
		"provider.rate-limit; token=private-value",
		"provider." + strings.Repeat("a", 60),
		"provider.",
		"ProviderRateLimit",
		"provider.rate\nlimit",
		"provider.sk-live-51h8xq2ez9abcdef",
		"provider.ghp-0123456789abcdef0123",
	} {
		line, err := json.Marshal(map[string]any{"type": "retry", "attempt": 1, "error": map[string]any{"type": raw, "status": 429}})
		if err != nil {
			t.Fatal(err)
		}
		obs := (opencodeJSONParser{}).Parse(line, false)
		if got := obs.Data["provider_error_type"]; got != "unclassified" {
			t.Fatalf("error type %q persisted as %#v", raw, got)
		}
	}
	valid := (opencodeJSONParser{}).Parse([]byte(`{"type":"retry","attempt":1,"error":{"type":"provider.rate-limit","status":429}}`), false)
	if valid.Data["provider_error_type"] != "provider.rate-limit" {
		t.Fatalf("known classifier namespace lost: %#v", valid.Data)
	}
}

func TestOpenCodeRetryThenIncompleteResultKeepsLocalDiagnostic(t *testing.T) {
	record := &processRecord{parser: opencodeJSONParser{}, done: make(chan struct{}), ref: SourceRef{Adapter: "opencode"}}
	record.ingest([]byte(`{"type":"retry","attempt":3,"error":{"type":"provider.rate-limit","status":429,"retryAfterMs":1000}}`), false)
	record.finish(nil)
	if record.result == nil || record.result.State != StateFailed {
		t.Fatalf("result = %#v", record.result)
	}
	if record.result.Data["diagnostic_code"] != "incomplete_terminal_result" {
		t.Fatalf("local completion diagnostic was replaced: %#v", record.result.Data)
	}
	if _, exists := record.result.Data["failure_category"]; exists {
		t.Fatalf("unresolved retry relabelled a local diagnostic: %#v", record.result.Data)
	}
	if _, exists := record.result.Data["http_status"]; exists {
		t.Fatalf("retry status leaked into the local failure diagnostic: %#v", record.result.Data)
	}
}

func TestOpenCodeRetryThenProviderErrorExitIsCapacity(t *testing.T) {
	record := &processRecord{parser: opencodeJSONParser{}, done: make(chan struct{}), ref: SourceRef{Adapter: "opencode"}}
	record.ingest([]byte(`{"type":"retry","attempt":2,"error":{"type":"provider.rate-limit","status":429,"retryAfterMs":26000}}`), false)
	record.ingest([]byte(`{"type":"error","error":{"type":"provider.rate-limit","message":"Rate limit exceeded","status":429}}`), false)
	record.finish(errors.New("exit status 1"))
	if record.result == nil || record.result.State != StateFailed || record.result.Data["failure_category"] != "capacity" || record.result.Data["http_status"] != int64(429) {
		t.Fatalf("provider error exit = %#v", record.result)
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
	if len(m.ProbeActions) != 1 || m.ProbeActions[0].Argv[1] != "--version" || m.ProbeActions[0].SideEffectClass != "read_only" || m.ProbeActions[0].TimeoutSeconds != 20 || m.VersionProbeTimeout() != 20*time.Second {
		t.Fatalf("probe actions = %#v", m.ProbeActions)
	}
	if len(m.KnownFailures) != 1 || m.KnownFailures[0].ErrorCode != "provider_capacity" {
		t.Fatalf("known failures = %#v", m.KnownFailures)
	}
	status := map[CapabilityName]CapabilityDeclaration{}
	for _, decl := range m.Capabilities {
		if _, duplicate := status[decl.Name]; duplicate {
			t.Fatalf("duplicate manifest capability %s", decl.Name)
		}
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
	for _, name := range []CapabilityName{CapabilityAttach, CapabilitySnapshot, CapabilityEvents, CapabilityResult, CapabilityResultContent} {
		if status[name].Constraints["scope"] != "same_process_only" || status[name].Constraints["cross_restart"] != false {
			t.Fatalf("%s constraints = %#v", name, status[name].Constraints)
		}
	}
	if status[CapabilityCancel].Constraints["scope"] != "live_owner" || status[CapabilityCancel].Constraints["cross_restart"] != false {
		t.Fatalf("cancel constraints = %#v", status[CapabilityCancel].Constraints)
	}
	if status[CapabilityEvents].Constraints["content"] != "metadata_only" {
		t.Fatalf("events constraints = %#v", status[CapabilityEvents].Constraints)
	}
	if status[CapabilityResume].Implementation != CapabilityUnavailable {
		t.Fatalf("resume = %#v", status[CapabilityResume])
	}
	content := status[CapabilityResultContent]
	if content.Constraints["max_bytes"] != opencodeAnswerLimit {
		t.Fatalf("manifest answer limit differs from buffer limit: %#v", content.Constraints)
	}
	if content.Constraints["required_output_mode"] != "json" || content.Constraints["required_subcommand"] != "run" || content.Constraints["required_terminal_event"] != "step_finish" || content.Constraints["required_finish_reason"] != "stop" {
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
	seen := map[CapabilityName]bool{}
	for _, capability := range got.Capabilities {
		if seen[capability.Name] {
			t.Fatalf("duplicate probe capability %s", capability.Name)
		}
		seen[capability.Name] = true
		if capability.Name == CapabilityResultContent && capability.Status == CapabilitySupported {
			found = true
		}
	}
	if !found {
		t.Fatalf("probe capabilities = %#v", got.Capabilities)
	}
}

// A clean process exit is not proof that the provider completed the final step.
func TestOpenCodeRequiresFinalStopStep(t *testing.T) {
	text := `'{"type":"text","sessionID":"s","part":{"type":"text","text":"partial answer","time":{"end":2}}}'`
	stop := `'{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"stop"}}'`
	start := `'{"type":"step_start","sessionID":"s","part":{"type":"step-start"}}'`
	for name, stream := range map[string]string{
		"missing finish":                          text,
		"unknown finish":                          text + ` '{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"unknown"}}'`,
		"length finish":                           text + ` '{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"length"}}'`,
		"tool calls finish":                       text + ` '{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"tool-calls"}}'`,
		"malformed finish part":                   text + ` '{"type":"step_finish","part":{"type":"text","reason":"stop"}}'`,
		"missing reason":                          text + ` '{"type":"step_finish","sessionID":"s","part":{"type":"step-finish"}}'`,
		"earlier stop cannot complete later step": text + " " + stop + " " + start + " " + text,
		"empty later unfinished step":             text + " " + stop + " " + start,
		"later text invalidates stop":             text + " " + stop + " " + text,
	} {
		t.Run(name, func(t *testing.T) {
			path := fixtureExecutable(t, "printf '%s\\n' "+stream)
			got, err := NewOpenCode().Launch(context.Background(), LaunchRequest{Argv: []string{path, "run", "--format", "json"}, DiscoveryWindow: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if got.Result == nil || got.Result.Success || got.Result.State != StateFailed || got.Result.Content != "" || got.Result.Data["diagnostic_code"] != "incomplete_terminal_result" || got.Result.ExitCode == nil || *got.Result.ExitCode != 0 {
				t.Fatalf("result = %#v", got.Result)
			}
		})
	}
}

func TestOpenCodeStepStartResetsStopAfterStreamLimit(t *testing.T) {
	record := &processRecord{parser: opencodeJSONParser{}, maxOutput: 1, done: make(chan struct{})}
	for _, line := range []string{
		`{"type":"text","part":{"type":"text","text":"old answer","time":{"end":1}}}`,
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}`,
		`{"type":"step_start","part":{"type":"step-start"}}`,
	} {
		record.ingest([]byte(line), false)
	}
	record.finish(nil)
	if record.result.Success || record.result.Content != "" || record.result.Data["diagnostic_code"] != "incomplete_terminal_result" {
		t.Fatalf("result = %#v", record.result)
	}
}

func TestOpenCodeCumulativeAnswerBufferIsBounded(t *testing.T) {
	for _, first := range []string{
		strings.Repeat("a", opencodeAnswerLimit),   // exact limit before another part
		strings.Repeat("a", opencodeAnswerLimit-3), // split a multibyte rune after the separator
	} {
		record := &processRecord{parser: opencodeJSONParser{}, maxOutput: 1, done: make(chan struct{})}
		text := func(value string) {
			t.Helper()
			line, err := json.Marshal(map[string]any{"type": "text", "part": map[string]any{"type": "text", "text": value, "time": map[string]int{"end": 1}}})
			if err != nil {
				t.Fatal(err)
			}
			record.ingest(line, false)
			if record.opencodeText.Len() > opencodeAnswerLimit || record.opencodeText.Cap() > 2*opencodeAnswerLimit {
				t.Fatalf("unbounded buffer: len=%d cap=%d", record.opencodeText.Len(), record.opencodeText.Cap())
			}
		}
		text(first)
		for i := 0; i < 128; i++ {
			text(strings.Repeat("🌍", 8192))
		}
		want := first
		if len(first) < opencodeAnswerLimit {
			want += "\n"
		}
		if got := record.opencodeText.String(); got != want || !utf8.ValidString(got) || !record.opencodeTruncated {
			t.Fatalf("prefix/truncation lost: len=%d want=%d truncated=%v", len(got), len(want), record.opencodeTruncated)
		}
		if len(record.observations) != 1 || record.observations[0].Content != "" || record.finalContent != "" {
			t.Fatalf("answer retained outside bounded buffer: observations=%d final=%d", len(record.observations), len(record.finalContent))
		}
		// Later parts cannot fill a UTF-8 boundary gap: retain a contiguous prefix.
		text("x")
		record.ingest([]byte(`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}`), false)
		record.finish(nil)
		if !record.result.Success || record.result.Content != want || !record.result.ContentTruncated {
			t.Fatalf("final stop lost after stream limit: %#v", record.result)
		}
	}
}

func TestOpenCodeFullBufferStillProcessesStepControls(t *testing.T) {
	for _, reason := range []string{"stop", "unknown", "tool-calls"} {
		t.Run(reason, func(t *testing.T) {
			record := &processRecord{parser: opencodeJSONParser{}, maxOutput: 1, done: make(chan struct{})}
			record.ingestObservation(parsedObservation{SourceState: "opencode.text", Content: strings.Repeat("a", opencodeAnswerLimit), Data: map[string]any{}})
			record.ingestObservation(parsedObservation{SourceState: "opencode.text", Content: "excess", Data: map[string]any{}})
			record.ingest([]byte(`{"type":"step_finish","part":{"type":"step-finish","reason":"tool-calls"}}`), false)
			if record.opencodeText.Len() != 0 || record.opencodeTruncated {
				t.Fatal("non-stop step did not clear the buffer")
			}
			record.ingest([]byte(`{"type":"step_start","part":{"type":"step-start"}}`), false)
			record.ingest([]byte(`{"type":"text","part":{"type":"text","text":"new answer","time":{"end":1}}}`), false)
			record.ingest([]byte(`{"type":"step_finish","part":{"type":"step-finish","reason":"`+reason+`"}}`), false)
			record.finish(nil)
			if reason == "stop" {
				if !record.result.Success || record.result.Content != "new answer" || record.result.ContentTruncated {
					t.Fatalf("new step did not recover: %#v", record.result)
				}
			} else if record.result.Success || record.result.Content != "" || record.result.Data["diagnostic_code"] != "incomplete_terminal_result" {
				t.Fatalf("non-stop final step succeeded: %#v", record.result)
			}
		})
	}
}

func TestOpenCodeInvalidRequestRetainsClassifier(t *testing.T) {
	record := &processRecord{parser: opencodeJSONParser{}, done: make(chan struct{}), ref: SourceRef{Adapter: "opencode"}}
	record.ingest([]byte(`{"type":"error","error":{"type":"provider.invalid-request","message":"private provider text"}}`), false)
	record.finish(nil)
	if record.result == nil || record.result.Data["diagnostic_code"] != "opencode_provider_invalid_request" {
		t.Fatalf("classifier lost: %+v", record.result)
	}
	if record.result.Content != "" {
		t.Fatal("failed output became answer")
	}
}
