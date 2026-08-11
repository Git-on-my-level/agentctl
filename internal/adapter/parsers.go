package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type codexParser struct{}

func (codexParser) Name() string { return "codex-json" }
func (codexParser) Parse(line []byte, stderr bool) parsedObservation {
	obs := parseAgentJSON(line, stderr, "codex", []string{"thread_id"}, []string{"turn.completed", "turn.failed", "error"})
	if value, ok := decodeLine(line); ok {
		if item, ok := value["item"].(map[string]any); ok && firstString(item, "type") == "agent_message" {
			obs.Content = boundedUTF8(firstString(item, "text"), 1<<20)
			obs.ContentType = "text/plain"
			obs.ContentTruncated = len(firstString(item, "text")) > len(obs.Content)
		}
	}
	return obs
}

type cursorParser struct{}

func (cursorParser) Name() string { return "cursor-stream-json" }
func (cursorParser) Parse(line []byte, stderr bool) parsedObservation {
	return parseAgentJSON(line, stderr, "cursor", []string{"session_id"}, []string{"result"})
}

type claudeParser struct{}

func (claudeParser) Name() string { return "claude-stream-json" }
func (claudeParser) Parse(line []byte, stderr bool) parsedObservation {
	return parseAgentJSON(line, stderr, "claude", []string{"session_id"}, []string{"result"})
}

type ompParser struct{}

func (ompParser) Name() string { return "omp-acp-json" }
func (ompParser) Parse(line []byte, stderr bool) parsedObservation {
	return parseAgentJSON(line, stderr, "omp", []string{"session_id", "session", "id"}, []string{"agent_end", "completed", "failed", "result", "error"})
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
			obs.ContentTruncated = len(result) > len(obs.Content)
		case map[string]any:
			if s := firstString(result, "summary", "message", "text"); s != "" {
				obs.Summary = boundedString(s, 2048)
				obs.Content = boundedUTF8(s, 1<<20)
				obs.ContentType = "text/plain"
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
		if containsAny(typ, "attention", "permission", "approval", "input") {
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
	return obs
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
