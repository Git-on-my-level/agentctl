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
	argv := []string{exe, "--print", "--verbose", "--output-format", "stream-json", "--model", model}
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryArgv}, nil
}
