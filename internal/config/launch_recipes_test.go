package config

import (
	"errors"
	"reflect"
	"testing"
)

func TestLaunchRecipesSeparateSyntaxAndRuntimeProvenance(t *testing.T) {
	profile := Profile{Multica: &Multica{Executable: "not-installed", Profile: "p", WorkspaceID: "w", ServerURL: "https://server", AppURL: "https://app"}, AgentPreferences: &AgentPreferences{Mode: "advisory", Preferred: []AgentPreference{
		{Agent: "devin", Model: "fusion-next-model", Speed: "regular"},
		{Agent: "claude", Model: "opus[1m]"},
		{Agent: "claude", Model: "opus[1m]", Speed: "regular"},
		{Agent: "future-harness", Model: "future-model"},
	}}}
	if err := validateProfile(profile); err != nil {
		t.Fatalf("syntax invalid: %v", err)
	}
	static := CheckLaunchRecipes(profile)
	if static.Compatible || static.Status != "incompatible" || len(static.Checks) != 4 || !static.Checks[0].Compatible || !static.Checks[1].Compatible || static.Checks[2].DiagnosticCode != "speed_unavailable" || static.Checks[3].DiagnosticCode != "unsupported_harness" {
		t.Fatalf("unexpected static report: %#v", static)
	}
	if again := CheckLaunchRecipes(profile); !reflect.DeepEqual(again, static) {
		t.Fatal("static report is nondeterministic")
	}
	provenance := CheckProfileProvenance(profile, ProvenanceOptions{ResolveExecutable: func(string) (string, error) { return "", errors.New("missing") }})
	if !provenance.SyntaxValid || provenance.Valid || !reflect.DeepEqual(provenance.LaunchRecipes, static) {
		t.Fatalf("syntax, provenance, and recipes were conflated: %#v", provenance)
	}
}

func TestLaunchRecipesDetectExecutableAliasConflict(t *testing.T) {
	profile := Profile{Adapters: map[string]Adapter{"claude": {Executable: "/one"}, "claude-code": {Executable: "/two"}}, AgentPreferences: &AgentPreferences{Mode: "advisory", Preferred: []AgentPreference{{Agent: "claude", Model: "opus"}}}}
	report := CheckLaunchRecipes(profile)
	if report.Compatible || report.Checks[0].DiagnosticCode != "ambiguous_executable" {
		t.Fatalf("conflicting executable aliases accepted: %#v", report)
	}
	profile.Adapters["claude-code"] = profile.Adapters["claude"]
	if report := CheckLaunchRecipes(profile); !report.Compatible {
		t.Fatalf("identical alias overrides rejected: %#v", report)
	}
}

func TestLaunchRecipesDoNotInventPermissionSupport(t *testing.T) {
	profile := Profile{Delegation: &DelegationPolicy{UnattendedCodingPermissions: true}, AgentPreferences: &AgentPreferences{Mode: "advisory", Preferred: []AgentPreference{{Agent: "omp", Model: "zai/glm-5.3"}, {Agent: "codex", Model: "exact-model"}, {Agent: "devin", Model: "exact-model"}}}}
	report := CheckLaunchRecipes(profile)
	if !report.Compatible || report.Checks[0].Permissions != "unsupported" || report.Checks[1].Permissions != "full" || report.Checks[2].Permissions != "full" {
		t.Fatalf("permission support misreported: %#v", report)
	}
	if got := CheckLaunchRecipes(Profile{}); !got.Compatible || got.Status != "not_configured" || len(got.Checks) != 0 {
		t.Fatalf("empty catalog misreported: %#v", got)
	}
}
