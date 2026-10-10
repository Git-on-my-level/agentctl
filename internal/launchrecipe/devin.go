package launchrecipe

import "strings"

// Devin print mode takes --model and -p. The reviewed preferred table owns
// exact model selection; native --model accepts a model argument without a
// source-code version allowlist. Fast and priority variants are refused.
// This does not attest account availability or native model identity.
// Print mode fails in an untrusted directory unless workspace trust
// is disabled, so the recipe passes that flag. The prompt is a positional
// argument after --.
//
// Print mode cannot answer an approval prompt: a tool call
// the mode does not auto-approve is rejected, its sibling calls are canceled,
// and devin exits 0 with only the text emitted so far. Unattended coding
// therefore needs --permission-mode dangerous. Read-only access is refused
// before this point: auto blocks writes but ends the run at the first
// rejected call, so a truncated answer would be recorded as success. "smart"
// is not offered by every model (Fusion rejects it), so it is never selected.
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
	argv := []string{exe, "--model", model, "-p", "--respect-workspace-trust", "false"}
	if applyCodingPermissions(access, grant) {
		argv = append(argv, "--permission-mode", "dangerous")
	}
	argv = append(argv, "--")
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryArgv}, nil
}

func devinModel(model string) error {
	lower := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(lower, "-fast") || strings.Contains(lower, "-priority") {
		return fail("model_unavailable", "devin fast and priority model variants are not accepted")
	}
	return nil
}
