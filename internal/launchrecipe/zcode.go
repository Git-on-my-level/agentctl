package launchrecipe

import "strings"

// ZCode has no verified --model or --thinking flag. Headless --json uses the
// CLI's configured model and reasoningLevel. ZCODE_MODEL selects a provisioned
// per-model config through the host-managed launcher; unsupported tuples fail.
func buildZCode(executable, model, speed, effort string) (Recipe, error) {
	exe, err := resolveExecutable("zcode", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed == "fast" {
		return Recipe{}, fail("speed_unavailable", "zcode cannot guarantee fast service speed")
	}
	nativeModel := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "zai/")
	if nativeModel != "glm-5.3" && nativeModel != "glm-5.3-flash" {
		return Recipe{}, fail("model_unavailable", "zcode launcher supports only zai/glm-5.3 and zai/glm-5.3-flash via provisioned ZCODE_MODEL configs")
	}
	if effort != "" && effort != "high" {
		return Recipe{}, fail("effort_unavailable", "zcode effort is the CLI reasoningLevel; only high is accepted because that is the configured default")
	}
	// Clear ambient selectors so the base model uses the configured default,
	// including on hosts that have no provisioned per-model config.
	env := []string{"ZCODE_MODEL="}
	if nativeModel == "glm-5.3-flash" {
		env[0] += nativeModel
	}
	return Recipe{Argv: []string{exe, "--json", "--mode", "yolo", "--prompt"}, PromptDelivery: PromptDeliveryArgv, Env: env}, nil
}
