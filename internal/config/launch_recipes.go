package config

import (
	"errors"
	"sort"

	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
)

// LaunchRecipeReport checks only whether the configured preferred tuples can
// be expressed by the compiled native recipes. It performs no executable
// lookup, native probe, account lookup, network request, or filesystem write.
// Syntax validity and executable provenance remain separate observations.
type LaunchRecipeReport struct {
	Compatible bool   `json:"compatible"`
	Status     string `json:"status"`
	// RuntimeVerified stays false: neither native nor Multica runtime checks
	// are part of this report. This includes provider/account readiness.
	RuntimeVerified bool                `json:"runtime_verified"`
	Checks          []LaunchRecipeCheck `json:"checks"`
	Limits          []string            `json:"limits"`
}

type LaunchRecipeCheck struct {
	Index          int    `json:"index"`
	Harness        string `json:"harness"`
	Model          string `json:"model"`
	Speed          string `json:"speed,omitempty"`
	Effort         string `json:"effort,omitempty"`
	Compatible     bool   `json:"compatible"`
	Permissions    string `json:"permissions,omitempty"`
	DiagnosticCode string `json:"diagnostic_code,omitempty"`
	Diagnostic     string `json:"diagnostic,omitempty"`
}

func CheckLaunchRecipes(profile Profile) LaunchRecipeReport {
	report := LaunchRecipeReport{Compatible: true, Status: "not_configured", Checks: []LaunchRecipeCheck{}, Limits: []string{
		"static native recipe compatibility only; no native version, authentication, account model availability, or provider identity is verified",
		"Multica runtime and assignee readiness are not checked; native recipes do not establish Multica delegation support",
		"checks use configured speed/effort and default coding access; request-specific settings may be unavailable",
	}}
	if profile.AgentPreferences == nil {
		return report
	}
	report.Status = "compatible"
	for i, preference := range profile.AgentPreferences.Preferred {
		check := LaunchRecipeCheck{Index: i, Harness: preference.Agent, Model: preference.Model, Speed: preference.Speed, Effort: preference.Effort}
		input := launchrecipe.Input{Harness: preference.Agent, Model: preference.Model, Speed: preference.Speed, Effort: preference.Effort}
		if profile.Delegation != nil {
			input.CursorWorkspaceTrust = profile.Delegation.CursorWorkspaceTrust
			input.UnattendedCodingPermissions = profile.Delegation.UnattendedCodingPermissions
		}
		harness, err := launchrecipe.CanonicalHarness(preference.Agent)
		if err == nil {
			check.Harness = harness
			// Match delegate's alias handling: conflicting executable overrides
			// make the launch ambiguous even when each individual recipe builds.
			values := map[string]bool{}
			for name, adapter := range profile.Adapters {
				canonical, canonicalErr := launchrecipe.CanonicalHarness(name)
				if canonicalErr == nil && canonical == harness && adapter.Executable != "" {
					values[adapter.Executable] = true
				}
			}
			if len(values) > 1 {
				err = &launchrecipe.Error{Code: "ambiguous_executable", Diagnostic: "harness aliases configure different executables"}
			} else {
				for executable := range values {
					input.Executable = executable
				}
			}
		}
		if err == nil {
			var recipe launchrecipe.Recipe
			recipe, err = launchrecipe.Build(input)
			if err == nil {
				check.Compatible = true
				check.Permissions = recipe.Permissions
			}
		}
		if err != nil {
			report.Compatible = false
			report.Status = "incompatible"
			var issue *launchrecipe.Error
			if errors.As(err, &issue) {
				check.DiagnosticCode = issue.Code
			}
			check.Diagnostic = err.Error()
		}
		report.Checks = append(report.Checks, check)
	}
	return report
}

// CheckConfigLaunchRecipes checks every composed profile in stable order.
func CheckConfigLaunchRecipes(cfg Config) map[string]LaunchRecipeReport {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	reports := make(map[string]LaunchRecipeReport, len(names))
	for _, name := range names {
		reports[name] = CheckLaunchRecipes(cfg.Profiles[name])
	}
	return reports
}
