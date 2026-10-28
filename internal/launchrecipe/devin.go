package launchrecipe

import "strings"

// acpConfigFlag asks agentctl's ACP client to set one session config option
// and read it back before the prompt. agentctl strips it before exec.
const acpConfigFlag = "--agentctl-acp-config"

// Devin delegates through ACP. Model selection remains configuration-owned.
// The ACP client verifies the session's model option equals the configured
// id, so a fuzzy native match fails instead of running another model.
func buildDevin(executable, model, speed, effort, access string, grant bool) (Recipe, error) {
	exe, err := devinPreflight(executable, model, speed, effort)
	if err != nil {
		return Recipe{}, err
	}
	argv := []string{exe, "acp", "--model", model, acpConfigFlag, "model=" + model}
	return Recipe{Argv: argv, Env: devinApprovalEnv(access, grant), PromptDelivery: PromptDeliveryStdin}, nil
}

// Devin cloud ignores `devin acp --cloud --model`; the cloud session selects
// its agent through the devin_version config option, which ACP sets and reads
// back before the first prompt. The repository option is added by the caller
// from the launch workspace.
func buildDevinCloud(executable, model, speed, effort, access string, grant bool) (Recipe, error) {
	exe, err := devinPreflight(executable, model, speed, effort)
	if err != nil {
		return Recipe{}, err
	}
	argv := []string{exe, "acp", "--cloud", acpConfigFlag, "devin_version=" + model}
	return Recipe{Argv: argv, Env: devinApprovalEnv(access, grant), PromptDelivery: PromptDeliveryStdin}, nil
}

func devinPreflight(executable, model, speed, effort string) (string, error) {
	exe, err := resolveExecutable("devin", executable)
	if err != nil {
		return "", err
	}
	if speed == "fast" {
		return "", fail("speed_unavailable", "devin launch recipes do not select fast service speed")
	}
	if effort != "" {
		return "", fail("effort_unavailable", "devin has no verified effort flag; the model id already names the reasoning variant")
	}
	if err := devinModel(model); err != nil {
		return "", err
	}
	return exe, nil
}

func devinApprovalEnv(access string, grant bool) []string {
	if applyCodingPermissions(access, grant) {
		return []string{"AGENTCTL_DEVIN_ACP_APPROVE=1"}
	}
	return []string{"AGENTCTL_DEVIN_ACP_APPROVE=0"}
}

func devinModel(model string) error {
	lower := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(lower, "-fast") || strings.Contains(lower, "-priority") {
		return fail("model_unavailable", "devin fast and priority model variants are not accepted")
	}
	return nil
}
