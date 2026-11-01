package adapter

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Hermes owns session policy. Stdin carries one query and is closed at EOF;
// neither text deltas nor stderr's human session footer are answer sources.
func NewHermes() Adapter {
	return &hermesAdapter{newNativeAdapter(nativeConfig{Manifest: hermesManifest(), Binary: "hermes", Parser: hermesParser{}, LaunchKind: "hermes_session"})}
}

type hermesAdapter struct{ *NativeAdapter }

func (h *hermesAdapter) Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
	if !hermesInvocationSatisfied(req.Argv) {
		return LaunchResult{}, invalidRequest("hermes requires chat --format stream-json --query-file - with managed stdin")
	}
	if err := validateHermesPrompt(req.PromptDelivery, req.Stdin); err != nil {
		return LaunchResult{}, err
	}
	return h.NativeAdapter.Launch(ctx, req)
}
func validateHermesPrompt(delivery string, prompt []byte) error {
	if delivery != PromptDeliveryStdin || len(prompt) == 0 || !utf8.Valid(prompt) {
		return invalidRequest("hermes requires a nonempty UTF-8 agentctl-managed stdin prompt")
	}
	return nil
}
func hermesInvocationSatisfied(argv []string) bool {
	if len(argv) < 2 || argv[1] != "chat" {
		return false
	}
	options := map[string]string{}
	for i := 2; i < len(argv); i++ {
		flag, value, inline := strings.Cut(argv[i], "=")
		switch flag {
		case "--format", "--query-file", "--model", "-m", "--provider", "--reasoning", "--max-turns", "--resume":
			if !inline {
				i++
				if i >= len(argv) {
					return false
				}
				value = argv[i]
			}
			if value == "" {
				return false
			}
		case "--yolo", "--quiet", "-Q", "--oneshot", "--no-restore-cwd":
			if inline {
				return false
			}
			value = "true"
		default:
			return false
		}
		if flag == "-m" {
			flag = "--model"
		}
		if flag == "-Q" {
			flag = "--quiet"
		}
		if _, exists := options[flag]; exists {
			return false
		}
		options[flag] = value
	}
	return options["--format"] == "stream-json" && options["--query-file"] == "-"
}
func hermesManifest() Manifest {
	result := resultContentDecl(CapabilitySupported, "assistant_terminal_result")
	result.Constraints["required_output_mode"] = "stream-json"
	result.Constraints["required_argv"] = map[string]any{"flag": "--format", "kind": "value", "value": "stream-json"}
	return baseManifest("hermes", "0.1.0", "hermes_session", "hermes-stream-json", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilitySupported), sameProcessDecl(CapabilityAttach, CapabilityDegraded), sameProcessDecl(CapabilitySnapshot, CapabilityDegraded), sameProcessDecl(CapabilityEvents, CapabilitySupported), sameProcessDecl(CapabilityResult, CapabilitySupported), result, liveOwnerDecl(CapabilityCancel, CapabilitySupported), capDecl(CapabilityContextInjection, CapabilityDegraded),
	})
}

type hermesParser struct{}

func (hermesParser) Name() string { return "hermes-stream-json" }
func (hermesParser) Parse(line []byte, stderr bool) parsedObservation {
	// Raw tool output and provider errors never enter metadata.
	obs := parsedObservation{Kind: "progress", State: StateRunning, Liveness: LivenessAlive, SourceState: "hermes.progress", Data: map[string]any{"family": "hermes"}}
	if stderr {
		obs.Kind = "health"
		obs.SourceState = "stderr"
		return obs
	}
	value, ok := decodeLine(line)
	if !ok {
		obs.Kind = "health"
		obs.SourceState = "malformed_output"
		return obs
	}
	typ := firstString(value, "type")
	switch typ {
	case "system":
		if firstString(value, "subtype") == "init" {
			obs.Kind = "started"
			obs.SourceState = "hermes.init"
			obs.SessionID = firstString(value, "session_id")
		}
	case "result":
		obs.Terminal, obs.Kind, obs.State, obs.Liveness = true, "terminal", StateFailed, LivenessExited
		obs.SourceState, obs.SessionID = "hermes.result", firstString(value, "session_id")
		code, valid := value["exit_code"].(float64)
		text, textOK := value["text"].(string)
		errorValue, hasError := value["error"]
		errorPresent := hasError && errorValue != nil
		if errorText, ok := errorValue.(string); ok && errorText == "" {
			errorPresent = false
		}
		if !valid || code != 0 || errorPresent {
			obs.Error = "Hermes reported a failed turn"
			obs.Data["diagnostic_code"] = "hermes_turn_failed"
			return obs
		}
		if !textOK || strings.TrimSpace(text) == "" {
			obs.Error = "Hermes exited without an assistant answer"
			obs.Data["diagnostic_code"] = "empty_terminal_result"
			return obs
		}
		obs.Success, obs.State = true, StateCompleted
		obs.Content, obs.ContentType, obs.ContentSource = boundedUTF8Exact(text, 1<<20), "text/plain", "assistant_terminal_result"
		obs.ContentTruncated = len(text) > len(obs.Content)
	}
	return obs
}

// Resume inherits the native session's frozen model, reasoning and permissions.
// Creation overrides (including expert --max-turns) must never be replayed.
func hermesResumeArgv(base []string, sessionID, delivery string) ([]string, error) {
	if delivery != PromptDeliveryStdin || !hermesInvocationSatisfied(base) {
		return nil, fmt.Errorf("hermes resume requires chat --format stream-json --query-file - and stdin delivery")
	}
	return []string{base[0], "chat", "--format", "stream-json", "--resume", sessionID, "--query-file", "-"}, nil
}
