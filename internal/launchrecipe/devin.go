package launchrecipe

import "strings"

// Devin delegates through ACP. Model selection remains configuration-owned.
func buildDevin(executable, model, speed, effort, access string, grant bool) (Recipe, error) {
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
	argv := []string{exe, "acp", "--model", model}
	env := []string{"AGENTCTL_DEVIN_ACP_APPROVE=0"}
	if applyCodingPermissions(access, grant) {
		env[0] = "AGENTCTL_DEVIN_ACP_APPROVE=1"
	}
	return Recipe{Argv: argv, Env: env, PromptDelivery: PromptDeliveryStdin}, nil
}

func devinModel(model string) error {
	lower := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(lower, "-fast") || strings.Contains(lower, "-priority") {
		return fail("model_unavailable", "devin fast and priority model variants are not accepted")
	}
	return nil
}
