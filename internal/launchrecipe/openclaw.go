package launchrecipe

import "strings"

// The prompt follows -m. Runtime launch adds a unique session key so separate
// delegations cannot share OpenClaw's default agent transcript.
func buildOpenClaw(executable, model, speed, effort string) (Recipe, error) {
	exe, err := resolveExecutable("openclaw", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed == "fast" {
		return Recipe{}, fail("speed_unavailable", "openclaw has no verified fast service-speed flag")
	}
	if !strings.Contains(model, "/") {
		return Recipe{}, fail("model_unavailable", "openclaw model must use provider/model syntax")
	}
	argv := []string{exe, "agent", "--local", "--agent", "main", "--model", model, "--json"}
	if effort != "" {
		switch effort {
		case "low", "medium", "high":
			argv = append(argv, "--thinking", effort)
		default:
			return Recipe{}, fail("effort_unavailable", "openclaw has no verified thinking level for this effort")
		}
	}
	argv = append(argv, "-m")
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryArgv}, nil
}
