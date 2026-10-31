package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/zcodeconfig"
)

var zcodeBuiltinPath = zcodeconfig.BuiltinPath

// resolveZCodeEffort reports a profile observation, not provider attestation.
// Read-only launches deliberately ignore the ambient personal-file override,
// matching prepareZCodeLaunch's isolated HOME setup.
func resolveZCodeEffort(settings *model.DelegationSettings, env []string, requested, cwd string) *output.Error {
	requested = strings.ToLower(strings.TrimSpace(requested))
	selector, override := "", os.Getenv("ZCODE_PERSONAL_PROVIDER_CONFIG_FILE")
	app, builtinOverride := os.Getenv("ZCODE_APP"), os.Getenv("ZCODE_BUILTIN_PROVIDER_CONFIG_FILE")
	readOnly := false
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "ZCODE_MODEL="); ok {
			selector = value
		}
		if entry == launchrecipe.ZCodeReadOnlyProfileEnv {
			readOnly = true
		}
		if value, ok := strings.CutPrefix(entry, "ZCODE_APP="); ok {
			app = value
		}
		if value, ok := strings.CutPrefix(entry, "ZCODE_BUILTIN_PROVIDER_CONFIG_FILE="); ok {
			builtinOverride = value
		}
	}
	if readOnly {
		override, app, builtinOverride = "", "", ""
	}
	home, err := os.UserHomeDir()
	path := ""
	if err == nil {
		path = zcodeconfig.ProviderPath(home, selector, override)
		if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
	}
	builtin := zcodeBuiltinPath(app, builtinOverride)
	if !filepath.IsAbs(builtin) {
		builtin = filepath.Join(cwd, builtin)
	}
	settings.Effort, settings.EffortWarning = zcodeconfig.Effort(path, builtin)
	if requested != "" && settings.Effort != requested {
		message := "ZCode selected profile reasoningLevel does not match the required effort; launch refused"
		if settings.Effort == "" {
			message += ": " + settings.EffortWarning
		}
		return delegateError(output.CodeCapabilityUnavailable, "zcode_effort_mismatch", message).
			WithDetail("effort", settings.Effort).WithDetail("effort_warning", settings.EffortWarning)
	}
	return nil
}
