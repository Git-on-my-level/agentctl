package launchrecipe

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestCursorRecipeFlags(t *testing.T) {
	got, err := Build(Input{
		Harness:              "cursor",
		Model:                "cursor-grok-4.6-high",
		Speed:                "regular",
		Executable:           "/opt/cursor-agent",
		CursorWorkspaceTrust: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/opt/cursor-agent", "--print", "--output-format", "stream-json",
		"--model", "cursor-grok-4.6-high", "--trust",
	}
	if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%v want=%v", got.Argv, want)
	}
	if got.PromptDelivery != PromptDeliveryArgv {
		t.Fatalf("prompt delivery=%q want argv", got.PromptDelivery)
	}
}

func TestCursorOmitsTrustWithoutGrant(t *testing.T) {
	got, err := Build(Input{Harness: "cursor", Model: "composer-2.5"})
	if err != nil {
		t.Fatal(err)
	}
	if contains(got.Argv, "--trust") {
		t.Fatalf("trust must not be injected without grant: %v", got.Argv)
	}
}

func TestCursorBracketEffortAndRegularSpeed(t *testing.T) {
	got, err := Build(Input{
		Harness: "cursor",
		Model:   "claude-opus-4-8[context=1m]",
		Speed:   "regular",
		Effort:  "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	model := got.Argv[5]
	if model != "claude-opus-4-8[context=1m,effort=high,fast=false]" {
		t.Fatalf("model arg=%q", model)
	}
}

func TestCursorEffortUnavailableForNonParameterizedModels(t *testing.T) {
	_, err := Build(Input{Harness: "cursor", Model: "composer-2.5", Effort: "high"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "effort_unavailable" {
		t.Fatalf("err=%v want effort_unavailable", err)
	}
}

func TestRejectFastModelWhenRegularRequested(t *testing.T) {
	_, err := Build(Input{Harness: "cursor", Model: "composer-2.5-fast", Speed: "regular"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "speed_model_mismatch" {
		t.Fatalf("err=%v want speed_model_mismatch", err)
	}
}

func TestRejectBracketFastTrueWhenRegularRequested(t *testing.T) {
	_, err := Build(Input{Harness: "cursor", Model: "claude-opus-4-8[fast=true]", Speed: "regular"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "speed_model_mismatch" {
		t.Fatalf("err=%v want speed_model_mismatch", err)
	}
}

func TestCodexRecipeUsesQuotedConfigOverrides(t *testing.T) {
	got, err := Build(Input{
		Harness: "codex",
		Model:   "gpt-5.6-sol",
		Speed:   "regular",
		Effort:  "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"codex", "exec", "--json", "--model", "gpt-5.6-sol"}
	if strings.Join(got.Argv[:5], "\x00") != strings.Join(wantPrefix, "\x00") {
		t.Fatalf("prefix=%v", got.Argv[:5])
	}
	if got.Argv[6] != "service_tier="+strconv.Quote("default") {
		t.Fatalf("service tier override=%q", got.Argv[5])
	}
	if got.Argv[8] != "model_reasoning_effort="+strconv.Quote("medium") {
		t.Fatalf("effort override=%q", got.Argv[6])
	}
	if got.Argv[len(got.Argv)-1] != "-" {
		t.Fatalf("terminal stdin sentinel=%q", got.Argv[len(got.Argv)-1])
	}
	if got.PromptDelivery != PromptDeliveryStdin {
		t.Fatalf("prompt delivery=%q want stdin", got.PromptDelivery)
	}
}

func TestCodexFastServiceTierOverride(t *testing.T) {
	got, err := Build(Input{Harness: "codex", Model: "gpt-5.6-sol", Speed: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Argv[6] != "service_tier="+strconv.Quote("priority") {
		t.Fatalf("fast tier override=%q", got.Argv[5])
	}
}

func TestCodexDoesNotInjectPromptBytes(t *testing.T) {
	got, err := Build(Input{Harness: "codex", Model: "gpt-5.6-sol"})
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range got.Argv {
		if strings.Contains(arg, "review this") {
			t.Fatalf("prompt bytes leaked into argv: %v", got.Argv)
		}
	}
}

func TestOMPRecipeFlags(t *testing.T) {
	got, err := Build(Input{Harness: "omp", Model: "zai/glm-5.3", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"omp", "--no-prewalk", "-p", "--mode", "json", "--model", "zai/glm-5.3", "--thinking", "high"}
	if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%v want=%v", got.Argv, want)
	}
	if contains(got.Argv, "--auto-approve") {
		t.Fatalf("permission broadening flag present: %v", got.Argv)
	}
}

func TestOMPRejectsSpeedGuarantee(t *testing.T) {
	_, err := Build(Input{Harness: "omp", Model: "zai/glm-5.3", Speed: "regular"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "speed_unavailable" {
		t.Fatalf("err=%v want speed_unavailable", err)
	}
}

func TestOMPRejectsEffortInModelSlug(t *testing.T) {
	_, err := Build(Input{Harness: "omp", Model: "zai/glm-5.3:high"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "effort_conflict" {
		t.Fatalf("err=%v want effort_conflict", err)
	}
}

func TestOMPRejectsUnsupportedThinkingEffort(t *testing.T) {
	_, err := Build(Input{Harness: "omp", Model: "zai/glm-5.3", Effort: "xhigh"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "effort_unavailable" {
		t.Fatalf("err=%v want effort_unavailable", err)
	}
}

func TestClaudeCodeStructuredOutputOnly(t *testing.T) {
	got, err := Build(Input{Harness: "claude-code", Model: "claude-sonnet-4"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--print", "--verbose", "--output-format", "stream-json", "--model", "claude-sonnet-4"}
	if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%v want=%v", got.Argv, want)
	}
}

func TestClaudeCodeRejectsEffort(t *testing.T) {
	_, err := Build(Input{Harness: "claude-code", Model: "claude-sonnet-4", Effort: "high"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "effort_unavailable" {
		t.Fatalf("err=%v want effort_unavailable", err)
	}
}

func TestUnsupportedHarnessRejected(t *testing.T) {
	_, err := Build(Input{Harness: "multica", Model: "anything"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "unsupported_harness" {
		t.Fatalf("err=%v want unsupported_harness", err)
	}
}

func TestHighSuffixIsNotEffortInference(t *testing.T) {
	got, err := Build(Input{Harness: "cursor", Model: "cursor-grok-4.6-high", Speed: "regular"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Argv[5], "effort=") {
		t.Fatalf("effort must not be inferred from -high slug: %v", got.Argv)
	}
}

func TestCodexConfigArgvIsSingleElementWithoutShellReparsing(t *testing.T) {
	got, err := Build(Input{Harness: "codex", Model: "gpt-5.6-sol", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	override := got.Argv[6]
	if override != "model_reasoning_effort="+strconv.Quote("high") {
		t.Fatalf("override=%q", override)
	}
	if got.Argv[5] != "-c" {
		t.Fatalf("config flag and TOML assignment must be separate argv elements: %q", override)
	}
}

func contains(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}

func TestRejectMalformedModelParametersWithoutChangingIdentity(t *testing.T) {
	for _, model := range []string{"model[fast=true]suffix", "model[fast=false,fast=true]", "model[broken]", "model[]", "model[[fast=false]]"} {
		if _, err := Build(Input{Harness: "cursor", Model: model}); err == nil {
			t.Fatalf("accepted %q", model)
		}
	}
}
