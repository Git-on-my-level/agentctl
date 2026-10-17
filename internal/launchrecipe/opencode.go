package launchrecipe

import "strings"

func buildOpenCode(executable, model, speed, effort string) (Recipe, error) {
	exe, err := resolveExecutable("opencode", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed != "" {
		return Recipe{}, fail("speed_unavailable", "opencode run has no verified service-speed flag; omit speed or choose another harness")
	}
	if effort != "" {
		return Recipe{}, fail("effort_unavailable", "opencode run has no verified effort flag; omit effort or choose another harness")
	}
	provider, slug, ok := strings.Cut(model, "/")
	if !ok || provider == "" || slug == "" || strings.ContainsAny(model, "[] \t\n") {
		return Recipe{}, fail("model_unavailable", "opencode run requires an exact provider/model id")
	}
	return Recipe{Argv: []string{exe, "run", "--format", "json", "--model", model, "--"}, PromptDelivery: PromptDeliveryArgv}, nil
}
