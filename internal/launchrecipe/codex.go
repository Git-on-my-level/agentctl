package launchrecipe

import (
	"fmt"
)

// Codex service-tier overrides are verified against ~/.codex/config.toml
// (service_tier = "default") and models_cache.json (tier id "priority", name "Fast").
func buildCodex(executable, model, speed, effort string) (Recipe, error) {
	exe, err := resolveExecutable("codex", executable)
	if err != nil {
		return Recipe{}, err
	}
	argv := []string{exe, "exec", "--json", "--model", model}
	switch speed {
	case "regular":
		argv = append(argv, "-c", codexConfigOverride("service_tier", "default"))
	case "fast":
		argv = append(argv, "-c", codexConfigOverride("service_tier", "priority"))
	}
	if effort != "" {
		if _, ok := codexEfforts[effort]; !ok {
			return Recipe{}, fail("effort_unavailable", fmt.Sprintf("codex does not support effort %q", effort))
		}
		argv = append(argv, "-c", codexConfigOverride("model_reasoning_effort", effort))
	}
	argv = append(argv, "-")
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryStdin}, nil
}
