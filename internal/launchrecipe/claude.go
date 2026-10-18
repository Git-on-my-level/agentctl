package launchrecipe

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
