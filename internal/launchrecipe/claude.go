package launchrecipe

// Claude Code is included with the adapter-manifest structured-output requirement
// only. Speed and effort guarantees are rejected until a verified native flag map
// exists for this harness.
func buildClaudeCode(executable, model, speed, effort string) (Recipe, error) {
	exe, err := resolveExecutable("claude-code", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed != "" {
		return Recipe{}, fail("speed_unavailable", "claude-code cannot guarantee explicit service speed in launch recipes")
	}
	if effort != "" {
		return Recipe{}, fail("effort_unavailable", "claude-code effort is not mapped to a verified native flag in launch recipes")
	}
	if isFastModelVariant(model) {
		return Recipe{}, fail("speed_model_mismatch", "fast model variants are not supported for claude-code launch recipes")
	}
	// Streaming input keeps native stdin open under agentctl, so a delegated
	// Claude Code session can take a steering message without being
	// interrupted. --replay-user-messages is the acknowledgement channel.
	argv := []string{exe, "--print", "--verbose", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages", "--model", model}
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryStream}, nil
}
