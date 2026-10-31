package launchrecipe

import (
	"regexp"
	"strconv"
	"strings"
)

// autoCompactWindowEnv is Claude Code's launch-scoped compaction window. It
// outranks --autocompact and every settings file, so one lane's window never
// changes the host default the way /autocompact does. Bare token counts work on
// every release that has the variable; k and M suffixes need a newer one.
const autoCompactWindowEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

const (
	minAutoCompactWindow = 100000
	maxAutoCompactWindow = 1000000
)

var autoCompactWindowPattern = regexp.MustCompile(`^([0-9]{1,7})([kKmM]?)$`)

// normalizeAutoCompactWindow returns the window as a bare token count. Unlike
// /autocompact, a small bare number is not read as thousands: it is refused.
func normalizeAutoCompactWindow(harness, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if harness != "claude-code" {
		return "", fail("auto_compact_window_unavailable", "settings.auto_compact_window is supported only for claude-code launch recipes")
	}
	match := autoCompactWindowPattern.FindStringSubmatch(value)
	if match == nil {
		return "", fail("invalid_auto_compact_window", "settings.auto_compact_window must be a token count such as 300000 or 300k")
	}
	tokens, err := strconv.Atoi(match[1])
	if err != nil {
		return "", fail("invalid_auto_compact_window", "settings.auto_compact_window must be a token count such as 300000 or 300k")
	}
	switch strings.ToLower(match[2]) {
	case "k":
		tokens *= 1000
	case "m":
		tokens *= 1000000
	}
	if tokens < minAutoCompactWindow || tokens > maxAutoCompactWindow {
		return "", fail("invalid_auto_compact_window", "settings.auto_compact_window must be from 100000 to 1000000 tokens")
	}
	return strconv.Itoa(tokens), nil
}

// Claude Code uses its native --effort flag for the reviewed reasoning-effort
// values. Service speed remains unsupported by launch recipes.
func buildClaudeCode(executable, model, speed, effort, access string, codingGrant bool) (Recipe, error) {
	exe, err := resolveExecutable("claude-code", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed != "" {
		return Recipe{}, fail("speed_unavailable", "claude-code cannot guarantee explicit service speed in launch recipes")
	}
	if effort != "" {
		switch effort {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return Recipe{}, fail("effort_unavailable", "claude-code --effort supports low, medium, high, xhigh, and max")
		}
	}
	if isFastModelVariant(model) {
		return Recipe{}, fail("speed_model_mismatch", "fast model variants are not supported for claude-code launch recipes")
	}
	// Streaming input keeps native stdin open under agentctl, so a delegated
	// Claude Code session can take a steering message without being
	// interrupted. --replay-user-messages is the acknowledgement channel.
	argv := []string{exe, "--print", "--verbose", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages", "--model", model}
	if effort != "" {
		// Keep effort directly after model so the reviewed setting has a stable,
		// documented position in the exact argv plan.
		argv = append(argv, "--effort", effort)
	}
	if applyCodingPermissions(access, codingGrant) {
		argv = append(argv, "--permission-mode", "auto")
	}
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryStream}, nil
}
