package launchrecipe

import "strings"

// ZCode has no verified --model or --thinking flag. Headless --json uses the
// CLI's configured model and reasoningLevel. A different model or effort is
// refused instead of silently ignored.
func buildZCode(executable, model, speed, effort string) (Recipe, error) {
	exe, err := resolveExecutable("zcode", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed == "fast" {
		return Recipe{}, fail("speed_unavailable", "zcode cannot guarantee fast service speed")
	}
	if !zcodeDefaultModel(model) {
		return Recipe{}, fail("model_unavailable", "zcode launch uses the CLI's configured model; only zai/glm-5.3 is accepted")
	}
	if effort != "" && effort != "high" {
		return Recipe{}, fail("effort_unavailable", "zcode effort is the CLI reasoningLevel; only high is accepted because that is the configured default")
	}
	return Recipe{Argv: []string{exe, "--json", "--mode", "yolo", "--prompt"}, PromptDelivery: PromptDeliveryArgv}, nil
}

func zcodeDefaultModel(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "zai/glm-5.3", "glm-5.3":
		return true
	default:
		return false
	}
}
