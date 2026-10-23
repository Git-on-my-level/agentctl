package launchrecipe

func buildHermes(executable, model, speed, effort, access string, grant bool) (Recipe, error) {
	exe, err := resolveExecutable("hermes", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed != "" {
		return Recipe{}, fail("speed_unavailable", "hermes has no reviewed service-speed flag; omit speed")
	}
	argv := []string{exe, "chat", "--format", "stream-json", "--model", model}
	if effort != "" {
		argv = append(argv, "--reasoning", effort)
	}
	if applyCodingPermissions(access, grant) {
		argv = append(argv, "--yolo")
	}
	argv = append(argv, "--query-file", "-")
	return Recipe{Argv: argv, PromptDelivery: PromptDeliveryStdin}, nil
}
