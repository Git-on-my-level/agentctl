package launchrecipe

func buildCursor(executable, model, speed, effort string, trust bool) (Recipe, error) {
	exe, err := resolveExecutable("cursor", executable)
	if err != nil {
		return Recipe{}, err
	}
	base, bracket := splitModelBrackets(model)
	if speed == "fast" {
		if !isFastModelVariant(model) {
			if !cursorBracketCapable(base) {
				return Recipe{}, fail("speed_unavailable", "cursor cannot guarantee fast service speed for this model id")
			}
			merged, mergeErr := mergeBracketParams(bracket, map[string]string{"fast": "true"})
			if mergeErr != nil {
				return Recipe{}, mergeErr
			}
			bracket = merged
		}
	}
	if speed == "regular" && cursorBracketCapable(base) {
		merged, mergeErr := mergeBracketParams(bracket, map[string]string{"fast": "false"})
		if mergeErr != nil {
			return Recipe{}, mergeErr
		}
		bracket = merged
	}
	if effort != "" {
		if !cursorBracketCapable(base) {
			return Recipe{}, fail("effort_unavailable", "cursor effort is only supported via bracket syntax on parameterized claude-* model ids")
		}
		merged, mergeErr := mergeBracketParams(bracket, map[string]string{"effort": effort})
		if mergeErr != nil {
			return Recipe{}, mergeErr
		}
		bracket = merged
	}
	modelArg := applyBracketModel(base, bracket)
	argv := []string{exe, "--print", "--output-format", "stream-json", "--model", modelArg}
	if trust {
		argv = append(argv, "--trust")
	}
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryArgv}, nil
}
