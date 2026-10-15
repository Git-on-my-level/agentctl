package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type codexParser struct{}

func (codexParser) Name() string { return "codex-json" }
func (codexParser) Parse(line []byte, stderr bool) parsedObservation {
	if failure, ok := codexStructuredFailure(line); ok {
		return failure
	}
	obs := parseAgentJSON(line, stderr, "codex", []string{"thread_id"}, []string{"turn.completed", "turn.failed", "error"})
	if value, ok := decodeLine(line); ok {
		if item, ok := value["item"].(map[string]any); ok && firstString(item, "type") == "agent_message" {
			obs.Content = boundedUTF8(firstString(item, "text"), 1<<20)
			obs.ContentType = "text/plain"
			obs.ContentSource = "assistant"
			obs.ContentTruncated = len(firstString(item, "text")) > len(obs.Content)
		}
	}
	return obs
}

// nativeDiagnosticCodePattern keeps provider codes in native_code. Free text
// stays in the bounded message.
var nativeDiagnosticCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// codexStructuredFailure reads a terminal Codex error from the exec stream.
// turn.failed carries error.message; session-shaped event_msg task_complete
// records may also carry error.codex_error_info. The observation stores neither
// the prompt nor any earlier assistant answer.
func codexStructuredFailure(line []byte) (parsedObservation, bool) {
	value, ok := decodeLine(line)
	if !ok {
		return parsedObservation{}, false
	}
	typ := strings.ToLower(firstString(value, "type"))
	var raw any
	var source string
	switch typ {
	case "turn.failed", "error":
		raw, source = value["error"], typ
	case "event_msg":
		payload, _ := value["payload"].(map[string]any)
		if payload == nil {
			return parsedObservation{}, false
		}
		payloadType := strings.ToLower(firstString(payload, "type"))
		if payloadType != "task_complete" && payloadType != "error" {
			return parsedObservation{}, false
		}
		raw, source = payload["error"], payloadType
	default:
		return parsedObservation{}, false
	}
	message, code := codexErrorParts(raw)
	if message == "" && code == "" {
		return parsedObservation{}, false
	}
	message = boundedUTF8(safeFailureDiagnostic(message), 200)
	if message == "" && code == "" {
		return parsedObservation{}, false
	}
	data := map[string]any{"family": "codex", "failure_structured": true}
	if code != "" {
		data["native_code"] = code
	}
	return parsedObservation{
		SessionID:   firstString(value, "thread_id"),
		Kind:        "terminal",
		SourceState: source,
		State:       StateFailed,
		Liveness:    LivenessExited,
		Terminal:    true,
		Error:       message,
		Data:        data,
	}, true
}

func codexErrorParts(raw any) (string, string) {
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value), ""
	case map[string]any:
		message := strings.TrimSpace(firstString(value, "message", "detail"))
		code := strings.ToLower(strings.TrimSpace(firstString(value, "codex_error_info", "code")))
		if !nativeDiagnosticCodePattern.MatchString(code) {
			code = ""
		}
		return message, code
	default:
		return "", ""
	}
}

type cursorParser struct{}

func (cursorParser) Name() string { return "cursor-stream-json" }
func (cursorParser) Parse(line []byte, stderr bool) parsedObservation {
	obs := parseAgentJSON(line, stderr, "cursor", []string{"session_id"}, []string{"result"})
	value, ok := decodeLine(line)
	if !ok {
		return obs
	}
	typ := strings.ToLower(firstString(value, "type"))
	switch typ {
	case "thinking", "assistant", "tool_call", "tool_result", "tool":
		// These envelopes describe a phase within the task. In particular,
		// thinking/subtype=completed and a tool's result/status describe only
		// that phase, even when the shared parser recognizes a terminal word.
		// A task result must not be synthesized from phase text or tool output.
		obs.Terminal, obs.Success = false, false
		obs.Kind, obs.State, obs.Liveness = "progress", StateRunning, LivenessAlive
		obs.Summary, obs.Content, obs.ContentType, obs.ContentSource, obs.Error = "", "", "", "", ""
		obs.ContentTruncated = false
	}
	if obs.Terminal && strings.TrimSpace(obs.Content) != "" {
		// Cursor's terminal result field is the final assistant answer, not
		// generic process output. Preserve both its semantic role and the
		// native envelope that supplied it.
		obs.ContentSource = "assistant_terminal_result"
	}
	if obs.Terminal && obs.Success && strings.TrimSpace(obs.Content) == "" {
		obs.Data["diagnostic_code"] = "empty_terminal_result"
	}
	switch typ {
	case "thinking":
		obs.Data["progress_phase"] = "thinking"
	case "system":
		obs.Data["progress_phase"] = "initializing"
	case "assistant":
		obs.Data["progress_phase"] = "assistant"
	case "tool_call", "tool_result", "tool":
		obs.Data["progress_phase"] = "tool"
	case "result":
		obs.Data["progress_phase"] = "completing"
	}
	if typ == "assistant" {
		message, ok := value["message"].(map[string]any)
		if !ok || !strings.EqualFold(firstString(message, "role"), "assistant") {
			return obs
		}
		content := cursorAssistantText(message["content"])
		if content != "" {
			obs.Content = boundedUTF8(content, 1<<20)
			obs.ContentType = "text/plain"
			obs.ContentSource = "assistant"
			obs.ContentTruncated = len(content) > len(obs.Content)
		}
	}
	return obs
}

func cursorAssistantText(value any) string {
	switch content := value.(type) {
	case string:
		return strings.TrimSpace(content)
	case []any:
		parts := make([]string, 0, len(content))
		for _, item := range content {
			part, ok := item.(map[string]any)
			if !ok || !strings.EqualFold(firstString(part, "type"), "text") {
				continue
			}
			if text := strings.TrimSpace(firstString(part, "text")); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

type zcodeParser struct{}

type zcodeFailureDiagnostic struct {
	Message    string
	Category   string
	NativeCode string
}

var (
	zcodeProviderCodePattern = regexp.MustCompile(`(?i)\bproviderCode\s*:\s*['\"]?([0-9]{1,32})`)
	zcodeBracketCodePattern  = regexp.MustCompile(`^\s*(?:ProviderBusinessError\s*:\s*)?\[([0-9]{1,32})\]`)
	zcodeHTTPStatusPattern   = regexp.MustCompile(`(?i)\b(?:responseStatus|HTTP(?:\s+status)?)\s*[:=]?\s*([0-9]{3})`)
	zcodeResetTimePattern    = regexp.MustCompile(`(?i)\breset(?:s)?\s+(?:at\s+)?([0-9]{4}-[0-9]{2}-[0-9]{2}\s+[0-9]{2}:[0-9]{2}:[0-9]{2})`)
)

func zcodeStderrFailure(raw string) *zcodeFailureDiagnostic {
	lower := strings.ToLower(raw)
	providerCode := zcodeProviderCodePattern.FindStringSubmatch(raw)
	httpStatus := zcodeHTTPStatusPattern.FindStringSubmatch(raw)
	if !strings.Contains(lower, "providerbusinesserror") && len(providerCode) != 2 && len(httpStatus) != 2 && !strings.Contains(lower, "usage limit reached") && !strings.Contains(lower, "quota") && !strings.Contains(lower, "rate limit") {
		return nil
	}
	return parseZCodeFailure(raw)
}

func parseZCodeFailure(raw string) *zcodeFailureDiagnostic {
	message := boundedUTF8(safeFailureDiagnostic(raw), 200)
	if message == "" {
		message = "ZCode provider execution failed"
	}
	diagnostic := &zcodeFailureDiagnostic{Message: message}
	if match := zcodeProviderCodePattern.FindStringSubmatch(raw); len(match) == 2 {
		diagnostic.NativeCode = match[1]
	} else if match := zcodeBracketCodePattern.FindStringSubmatch(raw); len(match) == 2 {
		diagnostic.NativeCode = match[1]
	}
	status := ""
	if match := zcodeHTTPStatusPattern.FindStringSubmatch(raw); len(match) == 2 {
		status = match[1]
	}
	// Only an explicit ZCode provider code or HTTP status promotes the signal
	// to capacity here. Generic quota/rate-limit wording stays available to the
	// shared classifier, which distinguishes quota_exceeded from capacity.
	if diagnostic.NativeCode == "1308" || status == "429" {
		diagnostic.Category = "capacity"
		diagnostic.Message = "ZCode provider usage limit reached"
		if match := zcodeResetTimePattern.FindStringSubmatch(raw); len(match) == 2 {
			diagnostic.Message += "; resets " + match[1]
		}
	}
	return diagnostic
}

func (zcodeParser) Name() string { return "zcode-json" }
func (zcodeParser) Parse(line []byte, stderr bool) parsedObservation {
	if stderr {
		return parsedObservation{Kind: "health", State: StateRunning, Liveness: LivenessAlive, SourceState: "stderr", Data: map[string]any{"stream": "stderr", "structured": false}}
	}
	value, ok := decodeZCodeDocument(line)
	if !ok {
		return parsedObservation{Kind: "health", State: StateRunning, Liveness: LivenessAlive, SourceState: "malformed_output", Data: map[string]any{"parse_error": "malformed structured output"}}
	}
	obs := parsedObservation{State: StateRunning, Liveness: LivenessAlive, Data: map[string]any{"family": "zcode"}, SourceState: "zcode.response"}
	obs.SessionID = firstString(value, "sessionId", "session_id")
	if errText := errorText(value); errText != "" {
		diagnostic := parseZCodeFailure(errText)
		obs.Error = diagnostic.Message
		obs.Data["failure_structured"] = true
		if diagnostic.NativeCode != "" {
			obs.Data["native_code"] = diagnostic.NativeCode
		}
		obs.Terminal = true
		obs.Success = false
		obs.State = StateFailed
		return obs
	}
	response := strings.TrimSpace(firstString(value, "response"))
	if response == "" {
		obs.Data["diagnostic_code"] = "empty_terminal_result"
		obs.Terminal = true
		obs.Success = false
		obs.State = StateFailed
		return obs
	}
	obs.Content = boundedUTF8(response, 1<<20)
	obs.ContentType = "text/plain"
	obs.ContentSource = "assistant_terminal_result"
	obs.ContentTruncated = len(response) > len(obs.Content)
	obs.Terminal = true
	obs.Success = true
	obs.State = StateCompleted
	return obs
}

// decodeZCodeDocument finds the `--json` result document in captured stdout.
// The CLI prints it last, but a long run may print banners, update notices, or
// log lines before it. The result is the LAST complete top-level JSON value
// that has the result shape and is followed only by whitespace (ANSI is
// stripped first). Candidates are the start of the output and every line-initial
// `{`, tried last to first. A truncated final document therefore yields no
// result instead of promoting an earlier object, and an earlier object (a log
// line, or a decoy result) never wins over the real final document.
func decodeZCodeDocument(raw []byte) (map[string]any, bool) {
	text := bytes.TrimSpace([]byte(stripANSI(string(raw))))
	starts := []int{}
	if len(text) > 0 && text[0] == '{' {
		starts = append(starts, 0)
	}
	for i := 0; i+1 < len(text); i++ {
		if text[i] == '\n' && text[i+1] == '{' {
			starts = append(starts, i+1)
		}
	}
	for i := len(starts) - 1; i >= 0; i-- {
		dec := json.NewDecoder(bytes.NewReader(text[starts[i]:]))
		var value map[string]any
		if err := dec.Decode(&value); err != nil || value == nil {
			continue
		}
		if len(bytes.TrimSpace(text[starts[i]+int(dec.InputOffset()):])) != 0 {
			continue
		}
		if looksLikeZCodeDocument(value) {
			return value, true
		}
	}
	return nil, false
}

// looksLikeZCodeDocument requires the shape of ZCode's `--json` result: a
// response (or error) together with the session and turn it belongs to. Log
// records carry a sessionId too, but always a timestamp/event and no response.
func looksLikeZCodeDocument(value map[string]any) bool {
	for _, key := range []string{"timestamp", "event", "level"} {
		if _, ok := value[key]; ok {
			return false
		}
	}
	if _, hasError := value["error"]; hasError {
		return true
	}
	session, _ := value["sessionId"].(string)
	turn, _ := value["turnId"].(string)
	_, hasResponse := value["response"].(string)
	return session != "" && turn != "" && hasResponse
}

// devinPrintTrailer is the last line of the one-time welcome banner observed
// from `devin -p` before the answer. Print mode is plain text, not JSON.
const devinPrintTrailer = "You're all set. Run devin to get started."

type devinParser struct{}

func (devinParser) Name() string { return "devin-print" }
func (devinParser) Parse(line []byte, stderr bool) parsedObservation {
	if stderr {
		return parsedObservation{Kind: "health", State: StateRunning, Liveness: LivenessAlive, SourceState: "stderr", Data: map[string]any{"stream": "stderr", "structured": false}}
	}
	// Print text is the answer candidate. Completion is decided when the process
	// is reaped, so cancellation, timeout, and a nonzero exit are not stamped
	// as success from stdout alone.
	obs := parsedObservation{State: StateRunning, Liveness: LivenessAlive, Data: map[string]any{"family": "devin"}, SourceState: "devin.print"}
	response := devinPrintAnswer(line)
	if response == "" {
		obs.Data["diagnostic_code"] = "empty_terminal_result"
		return obs
	}
	obs.Content = boundedUTF8(response, 1<<20)
	obs.ContentType = "text/plain"
	obs.ContentSource = "assistant_terminal_result"
	obs.ContentTruncated = len(response) > len(obs.Content)
	return obs
}

// devinPrintAnswer returns the answer from an observed `devin -p` stdout blob.
// A first-run welcome banner may precede it; the answer is the text after that
// banner. Later runs are only the answer.
func devinPrintAnswer(raw []byte) string {
	text := stripANSI(string(raw))
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if idx := strings.Index(text, devinPrintTrailer); idx >= 0 {
		text = text[idx+len(devinPrintTrailer):]
	}
	text = strings.TrimSpace(text)
	if strings.Contains(text, "Welcome to Devin CLI") || strings.Contains(text, "Logged in as ") {
		return ""
	}
	return text
}

func stripANSI(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == 0x1b && i+1 < len(value) && value[i+1] == '[' {
			j := i + 2
			for j < len(value) && value[j] < 0x40 {
				j++
			}
			if j < len(value) {
				i = j
				continue
			}
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

type claudeParser struct{}

func (claudeParser) Name() string { return "claude-stream-json" }
func (claudeParser) Parse(line []byte, stderr bool) parsedObservation {
	return parseAgentJSON(line, stderr, "claude", []string{"session_id"}, []string{"result"})
}

type ompParser struct{}

func (ompParser) Name() string { return "omp-acp-json" }
func (ompParser) Parse(line []byte, stderr bool) parsedObservation {
	obs := parseAgentJSON(line, stderr, "omp", []string{"session_id", "session", "id"}, []string{"agent_end", "completed", "failed", "result", "error"})
	value, ok := decodeLine(line)
	if !ok {
		return obs
	}
	typ := strings.ToLower(firstString(value, "type"))
	var message map[string]any
	switch typ {
	case "message_end":
		message, _ = value["message"].(map[string]any)
		if message != nil && !strings.EqualFold(firstString(message, "stopReason", "stop_reason"), "stop") {
			message = nil
		}
	case "agent_end":
		message = lastOMPAssistantMessage(value["messages"])
	}
	if message == nil || !strings.EqualFold(firstString(message, "role"), "assistant") {
		if typ == "agent_end" && obs.Success {
			obs.Data["diagnostic_code"] = "empty_terminal_result"
		}
		return obs
	}
	content := ompAssistantText(message["content"])
	if content != "" {
		obs.Content = boundedUTF8(content, 1<<20)
		obs.ContentType = "text/plain"
		obs.ContentSource = "assistant"
		obs.ContentTruncated = len(content) > len(obs.Content)
		if obs.Terminal {
			obs.ContentSource = "assistant_terminal_result"
		}
	}
	if obs.Terminal && obs.Success && strings.TrimSpace(obs.Content) == "" {
		obs.Data["diagnostic_code"] = "empty_terminal_result"
	}
	if !obs.Terminal {
		return obs
	}
	switch strings.ToLower(firstString(message, "stopReason", "stop_reason")) {
	case "error":
		obs.Success = false
		obs.State = StateFailed
		obs.Error = boundedString(firstNonEmpty(firstString(message, "errorMessage", "error_message"), "OMP assistant turn failed"), 1024)
	case "aborted":
		obs.Success = false
		obs.State = StateCancelled
		obs.Error = boundedString(firstNonEmpty(firstString(message, "errorMessage", "error_message"), "OMP assistant turn aborted"), 1024)
	}
	return obs
}

func lastOMPAssistantMessage(value any) map[string]any {
	messages, ok := value.([]any)
	if !ok {
		return nil
	}
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if ok && strings.EqualFold(firstString(message, "role"), "assistant") {
			return message
		}
	}
	return nil
}

func ompAssistantText(value any) string {
	switch content := value.(type) {
	case string:
		return strings.TrimSpace(content)
	case []any:
		parts := make([]string, 0, len(content))
		for _, item := range content {
			part, ok := item.(map[string]any)
			if !ok || !strings.EqualFold(firstString(part, "type"), "text") {
				continue
			}
			if text := strings.TrimSpace(firstString(part, "text")); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

type multicaParser struct{}

func (multicaParser) Name() string { return "multica-json" }
func (multicaParser) Parse(line []byte, stderr bool) parsedObservation {
	return parseAgentJSON(line, stderr, "multica", []string{"run_id", "run", "id"}, []string{"completed", "failed", "cancelled", "error"})
}

// multicaPageParser handles the bounded `event list` JSON page. It expands
// the page into structured observations without retaining the raw response.
// Long-running `event watch` output is intentionally not accepted here.
type multicaPageParser struct{}

func (multicaPageParser) Name() string { return "multica-event-list-json" }

func (multicaPageParser) Parse(line []byte, stderr bool) parsedObservation {
	if stderr {
		return parsedObservation{Kind: "health", State: StateRunning, Liveness: LivenessAlive, SourceState: "stderr", Data: map[string]any{"stream": "stderr", "structured": false}}
	}
	var root any
	if err := json.Unmarshal(line, &root); err != nil {
		return parsedObservation{Kind: "health", State: StateRunning, Liveness: LivenessAlive, SourceState: "malformed_output", Data: map[string]any{"parse_error": "malformed Multica event page"}}
	}
	page := &parsedPage{}
	var items []any
	switch value := root.(type) {
	case []any:
		items = value
	case map[string]any:
		page.NextCursor = boundedString(firstString(value, "next_cursor", "nextCursor", "cursor"), 256)
		for _, key := range []string{"events", "items", "data"} {
			if candidate, ok := value[key].([]any); ok {
				items = candidate
				break
			}
		}
		if len(items) == 0 && looksLikeMulticaEvent(value) {
			items = []any{value}
		}
	default:
		return parsedObservation{Page: page}
	}
	page.Scanned = len(items)
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			continue
		}
		// Workspace events are not process sessions. Do not promote an event's
		// durable `id` to the source OpaqueID; the adapter process binding remains
		// the page invocation while event identity is represented by source
		// position/cursor and the bounded payload fields below.
		observation := parseAgentJSON(encoded, false, "multica", nil, []string{"completed", "failed", "cancelled", "error"})
		// A page event's cursor belongs to Multica's workspace authority. Keep
		// the event sequence/source position instead of replacing it with the
		// adapter-local observation counter, which would replay events after a
		// filtered page or process restart.
		observation.CursorAuthority = true
		observation.Cursor = firstNonEmpty(observation.SourcePosition, observation.Cursor)
		page.Observations = append(page.Observations, observation)
	}
	return parsedObservation{Page: page}
}

func looksLikeMulticaEvent(value map[string]any) bool {
	for _, key := range []string{"type", "event", "kind", "status", "state", "aggregate_id", "run_id", "issue_id"} {
		if _, ok := value[key]; ok {
			return true
		}
	}
	return false
}

type genericParser struct{}

func (genericParser) Name() string { return "generic-process-json" }
func (genericParser) Parse(line []byte, stderr bool) parsedObservation {
	return parseAgentJSON(line, stderr, "process", []string{"session_id", "session", "id"}, []string{"result", "completed", "failed", "error"})
}

func parseAgentJSON(line []byte, stderr bool, family string, sessionKeys, terminalTypes []string) parsedObservation {
	value, ok := decodeLine(line)
	if !ok {
		if classified := classifyUnstructured(line, family); classified.Kind != "" {
			return classified
		}
		if stderr {
			return parsedObservation{Kind: "health", State: StateRunning, Liveness: LivenessAlive, SourceState: "stderr", Data: map[string]any{"stream": "stderr", "structured": false}}
		}
		return parsedObservation{Kind: "health", State: StateRunning, Liveness: LivenessAlive, SourceState: "malformed_output", Data: map[string]any{"parse_error": "malformed structured output"}}
	}
	obs := parsedObservation{State: StateRunning, Liveness: LivenessAlive, Data: map[string]any{"family": family}}
	obs.SessionID = firstString(value, sessionKeys...)
	if obs.SessionID == "" {
		if nested, ok := value["session"].(map[string]any); ok {
			obs.SessionID = firstString(nested, "id", "session_id")
		}
	}
	if v := firstString(value, "backend_version", "version", "cli_version"); v != "" {
		obs.BackendVersion = boundedString(v, 128)
	}
	typ := strings.ToLower(firstString(value, "type", "event", "kind", "subtype", "status"))
	obs.SourceState = boundedString(typ, 128)
	if obs.SourceState == "" {
		obs.SourceState = family + ".observation"
	}
	obs.Cursor = boundedString(firstString(value, "cursor", "next_cursor"), 256)
	obs.SourcePosition = boundedString(firstString(value, "sequence", "position", "event_id", "id"), 256)
	if occurred := firstString(value, "occurred_at", "occurredAt", "timestamp"); occurred != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, occurred); err == nil {
			obs.OccurredAt = &parsed
		}
	}
	if errText := errorText(value); errText != "" {
		obs.Error = boundedString(errText, 1024)
	}
	isError, hasError := boolValue(value, "is_error", "error")
	if obs.Error != "" {
		// A structured error field is terminal failure evidence unless an
		// explicit success boolean below overrides it. This covers CLIs that
		// omit a separate `is_error` flag on result records.
		isError = true
	}
	if hasError && isError {
		obs.Error = firstNonEmpty(obs.Error, "native agent returned an error")
	}
	resultValue, hasResult := value["result"]
	if hasResult {
		switch result := resultValue.(type) {
		case string:
			obs.Summary = boundedString(result, 2048)
			obs.Content = boundedUTF8(result, 1<<20)
			obs.ContentType = "text/plain"
			obs.ContentSource = "terminal_result"
			obs.ContentTruncated = len(result) > len(obs.Content)
		case map[string]any:
			if s := firstString(result, "summary", "message", "text"); s != "" {
				obs.Summary = boundedString(s, 2048)
				obs.Content = boundedUTF8(s, 1<<20)
				obs.ContentType = "text/plain"
				obs.ContentSource = "terminal_result"
				obs.ContentTruncated = len(s) > len(obs.Content)
			}
		}
	}
	if s := firstString(value, "summary", "message"); s != "" && obs.Summary == "" {
		obs.Summary = boundedString(s, 2048)
	}
	status := observationStatus(value)
	explicitTerminal := containsAny(typ, terminalTypes...) || isTerminalStatus(status)
	if explicitSuccess, hasSuccess := boolValue(value, "success"); hasSuccess && (typ == "" || containsAny(typ, "result", "complete", "fail", "error")) {
		hasResult = true
		isError = !explicitSuccess
		explicitTerminal = true
	}
	if family == "process" && typ == "" && hasResult {
		explicitTerminal = true
	}
	terminal, success := false, false
	if explicitTerminal {
		terminal = true
		success = !isError && !containsAny(typ, "failed", "error", "cancelled", "canceled")
		if status != "" {
			switch status {
			case "success", "succeeded", "completed", "complete", "done":
				success = true
			case "failed", "failure", "error", "cancelled", "canceled":
				success = false
			}
		}
		if success {
			obs.State = StateCompleted
		} else if containsAny(typ, "cancel") || containsAny(strings.ToLower(firstString(value, "status", "state")), "cancel") {
			obs.State = StateCancelled
		} else {
			obs.State = StateFailed
		}
		obs.Liveness = LivenessExited
	}
	if !terminal {
		obs.Summary = ""
		obs.Content = ""
		obs.ContentType = ""
		obs.ContentTruncated = false
		if containsAny(typ, "attention", "permission", "approval", "input") || family == "multica" && multicaAttentionStatus(status) {
			obs.State = StateAttention
		}
		if containsAny(typ, "waiting", "wait") {
			obs.State = StateWaiting
		}
	}
	if typ == "" {
		typ = "progress"
	}
	if terminal {
		obs.Kind = "terminal"
	} else if containsAny(typ, "start", "init", "created") {
		obs.Kind = "started"
	} else if containsAny(typ, "artifact") {
		obs.Kind = "artifact"
	} else if obs.State == StateAttention {
		obs.Kind = "attention"
	} else {
		obs.Kind = "progress"
	}
	obs.Terminal, obs.Success = terminal, success
	obs.Data = safeObservationData(value, obs.SessionID, obs.BackendVersion, typ, family)
	if obs.State == StateAttention {
		attentionKind := "unknown"
		switch {
		case containsAny(typ, "permission"):
			attentionKind = "permission"
		case containsAny(typ, "approval"):
			attentionKind = "approval"
		case containsAny(typ, "authentication", "login"):
			attentionKind = "authentication"
		case containsAny(typ, "input"):
			attentionKind = "input"
		case containsAny(typ, "quota", "rate_limit"):
			attentionKind = "quota"
		}
		obs.Data["attention_kind"] = attentionKind
		obs.Data["diagnostic_code"] = normalizedDiagnosticCode(typ)
		if family == "multica" && multicaAttentionStatus(status) {
			if status == "in_review" || status == "in-review" || status == "review" || status == "needs_review" {
				obs.Data["attention_kind"] = "review"
			} else if status == "blocked" {
				obs.Data["attention_kind"] = "blocked"
			}
			obs.Data["diagnostic_code"] = normalizedDiagnosticCode("multica_" + status)
		}
	}
	return obs
}

func multicaAttentionStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "in_review", "in-review", "review", "needs_review", "blocked", "needs_input", "attention":
		return true
	default:
		return false
	}
}

func normalizedDiagnosticCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case out.Len() != 0 && !strings.HasSuffix(out.String(), "_"):
			out.WriteByte('_')
		}
		if out.Len() >= 128 {
			break
		}
	}
	return strings.Trim(out.String(), "_")
}

func observationStatus(value map[string]any) string {
	if status := strings.ToLower(firstString(value, "status", "state", "subtype")); status != "" {
		return status
	}
	for _, key := range []string{"payload", "data", "entity", "aggregate"} {
		container, ok := value[key].(map[string]any)
		if !ok {
			continue
		}
		if status := strings.ToLower(firstString(container, "status", "state", "subtype")); status != "" {
			return status
		}
	}
	return ""
}

func isTerminalStatus(status string) bool {
	switch status {
	case "success", "succeeded", "completed", "complete", "done", "failed", "failure", "error", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func safeObservationData(value map[string]any, sessionID, version, typ, family string) map[string]any {
	out := map[string]any{"family": family}
	if typ != "" {
		out["type"] = boundedString(typ, 128)
	}
	if sessionID != "" {
		out["session_id"] = boundedString(sessionID, 256)
	}
	if version != "" {
		out["backend_version"] = boundedString(version, 128)
	}
	for _, key := range []string{"is_error", "status", "state", "subtype", "usage", "artifact_ref", "run_id", "issue_id", "workspace", "workspace_id", "project_id", "aggregate_id", "aggregate_type", "aggregate_kind", "entity_id", "task_id", "execution_id", "sequence", "cursor", "next_cursor"} {
		if v, ok := value[key]; ok && safeScalar(v) {
			out[key] = v
		}
	}
	for _, containerKey := range []string{"payload", "data", "entity", "aggregate"} {
		container, ok := value[containerKey].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"run_id", "issue_id", "aggregate_id", "aggregate_type", "aggregate_kind", "entity_id", "task_id", "execution_id", "workspace_id", "state", "status"} {
			if v, ok := container[key]; ok && safeScalar(v) {
				out[key] = v
			}
		}
	}
	return out
}
func safeScalar(v any) bool {
	switch v.(type) {
	case nil, bool, float64, int, int64, string:
		return true
	default:
		return false
	}
}
func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := value[key]; ok {
			switch x := v.(type) {
			case string:
				if strings.TrimSpace(x) != "" {
					return x
				}
			case float64:
				return fmt.Sprintf("%.0f", x)
			}
		}
	}
	return ""
}
func boolValue(value map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		v, ok := value[key]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case bool:
			return x, true
		case map[string]any:
			return true, true
		case string:
			switch strings.ToLower(x) {
			case "true", "yes", "failed", "error":
				return true, true
			case "false", "no":
				return false, true
			}
		}
	}
	return false, false
}
func errorText(value map[string]any) string {
	for _, key := range []string{"error", "err", "failure"} {
		if v, ok := value[key]; ok {
			switch x := v.(type) {
			case string:
				if strings.TrimSpace(x) != "" {
					return x
				}
			case map[string]any:
				if s := firstString(x, "message", "detail", "error"); s != "" {
					return s
				}
			case bool:
				if x {
					return "native agent returned an error"
				}
			}
		}
	}
	return ""
}
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func containsAny(value string, choices ...string) bool {
	for _, choice := range choices {
		if strings.Contains(value, strings.ToLower(choice)) {
			return true
		}
	}
	return false
}
func boundedString(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max]
}

func boundedUTF8(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || len(value) <= max {
		return value
	}
	value = value[:max]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func classifyUnstructured(line []byte, family string) parsedObservation {
	text := strings.ToLower(string(bytes.TrimSpace(line)))
	code, kind := "", ""
	switch {
	case strings.Contains(text, "workspace") && strings.Contains(text, "trust"):
		code, kind = "workspace_trust_required", "permission"
	case strings.Contains(text, "sign in") || strings.Contains(text, "login required") || strings.Contains(text, "authentication required"):
		code, kind = "authentication_required", "authentication"
	case strings.Contains(text, "approval") || strings.Contains(text, "permission required"):
		code, kind = "approval_required", "approval"
	}
	if code == "" {
		return parsedObservation{}
	}
	return parsedObservation{Kind: "attention", SourceState: code, State: StateAttention, Liveness: LivenessAlive, Data: map[string]any{"family": family, "attention_kind": kind, "diagnostic_code": code}}
}
