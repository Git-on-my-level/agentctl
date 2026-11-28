package launchrecipe

import (
	"fmt"
)

// Codex service-tier overrides are verified against ~/.codex/config.toml
// (service_tier = "default") and models_cache.json (tier id "priority", name "Fast").
func buildCodex(executable, model, speed, effort, access string, grant bool) (Recipe, error) {
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
	if access == AccessReadOnly {
		argv = append(argv, "--sandbox", "read-only", "-c", codexConfigOverride("approval_policy", "never"))
	} else {
		// `codex exec` is headless and Codex 0.170.0 forces approval_policy=never
		// for its default human reviewer. Make that behavior explicit; the native
		// adapter supplies a launch-scoped allowlist for the granted commands.
		argv = append(argv, "--sandbox", "workspace-write", "-c", codexConfigOverride("approval_policy", "never"))
	}
	argv = append(argv, "--skip-git-repo-check", "-")
	var env []string
	if applyCodingPermissions(access, grant) {
		env = append(env, "AGENTCTL_CODEX_DELEGATE_ALLOWLIST=1")
	}
	return Recipe{Env: env, Argv: argv, PromptDelivery: PromptDeliveryStdin}, nil
}
