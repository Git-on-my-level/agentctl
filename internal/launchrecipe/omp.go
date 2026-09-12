package launchrecipe

import "strings"

// OMP --thinking values are bounded to the effort ladder verified in delegation
// skills (low/medium/high). OMP service-speed guarantees are not emitted.
func buildOMP(executable, model, speed, effort string) (Recipe, error) {
	exe, err := resolveExecutable("omp", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed != "" {
		return Recipe{}, fail("speed_unavailable", "omp cannot guarantee explicit service speed; omit speed or choose another harness")
	}
	if modelHasEffortSuffix(model) {
		return Recipe{}, fail("effort_conflict", "omp model ids cannot encode effort in slug suffixes; use --thinking")
	}
	argv := []string{exe, "--no-prewalk", "-p", "--mode", "json", "--model", model}
	if effort != "" {
		switch effort {
		case "low", "medium", "high":
			argv = append(argv, "--thinking", effort)
		default:
			return Recipe{}, fail("effort_unavailable", "omp --thinking only supports low, medium, and high effort guarantees")
		}
	}
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryArgv}, nil
}

func modelHasEffortSuffix(model string) bool {
	// Fail closed on provider/model:effort slug forms; effort must be explicit.
	idx := strings.LastIndex(model, ":")
	if idx < 0 || idx == len(model)-1 {
		return false
	}
	suffix := strings.ToLower(strings.TrimSpace(model[idx+1:]))
	switch suffix {
	case "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	default:
		return false
	}
}
