package launchrecipe

import "strings"

func buildOpenCode(executable, model, speed, effort, access string, unattendedCodingPermissions bool) (Recipe, error) {
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
	argv := []string{exe, "run", "--format", "json", "--model", model}
	if applyCodingPermissions(access, unattendedCodingPermissions) {
		argv = append(argv, "--auto")
	}
	argv = append(argv, "--")
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryArgv}, nil
}
