package launchrecipe

import "strings"

// Devin print mode takes --model and -p. Account ids are the two non-fast
// slugs observed from `devin models list`. Fast and priority variants are
// refused. Print mode fails in an untrusted directory unless workspace trust
// is disabled, so the recipe passes that flag. The prompt is a positional
// argument after --.
func buildDevin(executable, model, speed, effort string) (Recipe, error) {
	exe, err := resolveExecutable("devin", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed == "fast" {
		return Recipe{}, fail("speed_unavailable", "devin launch recipes do not select fast service speed")
	}
	if effort != "" {
		return Recipe{}, fail("effort_unavailable", "devin has no verified effort flag; the model id already names the reasoning variant")
	}
	if err := devinModel(model); err != nil {
		return Recipe{}, err
	}
	return Recipe{Argv: []string{exe, "--model", model, "-p", "--respect-workspace-trust", "false", "--"}, PromptDelivery: PromptDeliveryArgv}, nil
}

func devinModel(model string) error {
	switch model {
	case "swe-2-high", "fusion-gpt-6-sol-high-sidekick-swe-2-high":
		return nil
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(lower, "-fast") || strings.Contains(lower, "-priority") {
		return fail("model_unavailable", "devin fast and priority model variants are not accepted")
	}
	return fail("model_unavailable", "devin launch only accepts swe-2-high and fusion-gpt-6-sol-high-sidekick-swe-2-high")
}
