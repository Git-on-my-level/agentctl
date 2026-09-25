package adapter

import "strings"

// openclawParser reads the single JSON response from `agent --local --json`.
// Only visible assistant payload text is eligible as result content.
type openclawParser struct{}

func (openclawParser) Name() string { return "openclaw-json" }

func (openclawParser) Parse(raw []byte, stderr bool) parsedObservation {
	if stderr {
		return parsedObservation{Kind: "health", State: StateRunning, Liveness: LivenessAlive, SourceState: "stderr", Data: map[string]any{"stream": "stderr", "structured": false}}
	}
	value, ok := decodeLine(raw)
	if !ok {
		return parsedObservation{Kind: "health", State: StateRunning, Liveness: LivenessAlive, SourceState: "malformed_output", Data: map[string]any{"diagnostic_code": "malformed_structured_output"}}
	}
	obs := parsedObservation{Kind: "terminal", SourceState: "openclaw.agent", Terminal: true, State: StateCompleted, Success: true, Data: map[string]any{"family": "openclaw"}}
	obs.SessionID = firstString(value, "sessionId", "session_id")
	if meta, ok := value["meta"].(map[string]any); ok && obs.SessionID == "" {
		obs.SessionID = firstString(meta, "sessionId", "session_id")
	}
	status := strings.ToLower(firstString(value, "status"))
	if status == "error" || status == "failed" || status == "in_flight" {
		obs.Success, obs.State = false, StateFailed
		obs.Error = safeFailureDiagnostic(firstNonEmpty(errorText(value), "OpenClaw agent returned status "+status))
		return obs
	}
	if errText := errorText(value); errText != "" {
		obs.Success, obs.State = false, StateFailed
		obs.Error = safeFailureDiagnostic(errText)
		return obs
	}
	if meta, ok := value["meta"].(map[string]any); ok {
		if errText := errorText(meta); errText != "" {
			obs.Success, obs.State = false, StateFailed
			obs.Error = safeFailureDiagnostic(errText)
			return obs
		}
	}
	var parts []string
	if payloads, ok := value["payloads"].([]any); ok {
		for _, item := range payloads {
			payload, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if failed, _ := payload["isError"].(bool); failed {
				obs.Success, obs.State = false, StateFailed
				obs.Error = safeFailureDiagnostic(firstNonEmpty(firstString(payload, "text"), "OpenClaw agent returned an error payload"))
				return obs
			}
			if part := strings.TrimSpace(firstString(payload, "text")); part != "" {
				parts = append(parts, part)
			}
		}
	}
	answer := strings.Join(parts, "\n\n")
	if answer == "" {
		obs.Success, obs.State = false, StateFailed
		obs.Error = "OpenClaw agent exited without a text answer"
		obs.Data["diagnostic_code"] = "empty_terminal_result"
		return obs
	}
	obs.Content = boundedUTF8(answer, 1<<20)
	obs.ContentType = "text/plain"
	obs.ContentSource = "assistant_terminal_result"
	obs.ContentTruncated = len(answer) > len(obs.Content)
	return obs
}
