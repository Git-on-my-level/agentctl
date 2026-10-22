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

func TestClaudeCodeUsesSteerableStreamingInput(t *testing.T) {
	got, err := Build(Input{Harness: "claude-code", Model: "claude-sonnet-4"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--print", "--verbose", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages", "--model", "claude-sonnet-4"}
	if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%v want=%v", got.Argv, want)
	}
	if got.PromptDelivery != PromptDeliveryStream {
		t.Fatalf("prompt delivery=%q want stream", got.PromptDelivery)
	}
}

func TestClaudeCodeMapsSupportedEffortValues(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		t.Run(effort, func(t *testing.T) {
			got, err := Build(Input{Harness: "claude-code", Model: "claude-sonnet-4", Effort: effort})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"claude", "--print", "--verbose", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages", "--model", "claude-sonnet-4", "--effort", effort}
			if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("argv=%v want=%v", got.Argv, want)
			}
		})
	}
}

func TestClaudeCodeOmitsEffortWhenUnspecified(t *testing.T) {
	got, err := Build(Input{Harness: "claude-code", Model: "claude-sonnet-4"})
	if err != nil {
		t.Fatal(err)
	}
	if contains(got.Argv, "--effort") {
		t.Fatalf("unspecified effort injected: %v", got.Argv)
	}
}

func TestClaudeCodeRejectsCodexOnlyEffort(t *testing.T) {
	for _, effort := range []string{"ultra", "super"} {
		_, err := Build(Input{Harness: "claude-code", Model: "claude-sonnet-4", Effort: effort})
		var buildErr *Error
		if !errors.As(err, &buildErr) || buildErr.Code != "effort_unavailable" {
			t.Fatalf("effort=%q err=%v want effort_unavailable", effort, err)
		}
	}
}

func TestClaudeCodeSpeedAndFastVariantRejectionsRemain(t *testing.T) {
	_, err := Build(Input{Harness: "claude-code", Model: "claude-sonnet-4", Speed: "regular"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "speed_unavailable" {
		t.Fatalf("speed error=%v want speed_unavailable", err)
	}
	_, err = Build(Input{Harness: "claude-code", Model: "claude-sonnet-4-fast"})
	if !errors.As(err, &buildErr) || buildErr.Code != "speed_model_mismatch" {
		t.Fatalf("fast variant error=%v want speed_model_mismatch", err)
	}
}

func TestZCodeRecipeUsesProvisionedModel(t *testing.T) {
	got, err := Build(Input{Harness: "zcode", Model: "zai/glm-5.3", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"zcode", "--json", "--mode", "yolo", "--prompt"}
	if len(got.Argv) != len(want) {
		t.Fatalf("argv = %#v", got.Argv)
	}
	for i := range want {
		if got.Argv[i] != want[i] {
			t.Fatalf("argv = %#v", got.Argv)
		}
	}
	if got.PromptDelivery != PromptDeliveryArgv {
		t.Fatalf("delivery = %q", got.PromptDelivery)
	}
	for _, model := range []string{"glm-5.3", "glm-5.3-flash"} {
		selected, err := Build(Input{Harness: "zcode", Model: "zai/" + model})
		selector := "ZCODE_MODEL="
		if model == "glm-5.3-flash" {
			selector += model
		}
		if err != nil || len(selected.Env) != 1 || selected.Env[0] != selector {
			t.Fatalf("model=%s env=%v err=%v", model, selected.Env, err)
		}
	}
	if _, err := Build(Input{Harness: "zcode", Model: "zai/glm-4.7"}); err == nil {
		t.Fatal("expected a different model to be refused")
	}
	if _, err := Build(Input{Harness: "zcode", Model: "zai/glm-5.3", Effort: "low"}); err == nil {
		t.Fatal("expected a non-high effort to be refused")
	}
}

func TestDevinRecipePassesModelAndACP(t *testing.T) {
	for _, model := range []string{"swe-2-high", "fusion-gpt-6-sol-high-sidekick-swe-2-high", "fusion-next-model", "claude-sonnet-4"} {
		got, err := Build(Input{Harness: "devin", Model: model})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"devin", "acp", "--model", model}
		if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") || got.PromptDelivery != PromptDeliveryStdin {
			t.Fatalf("argv=%v delivery=%s", got.Argv, got.PromptDelivery)
		}
	}
	explicit, err := Build(Input{Harness: "devin", Model: "swe-2-high", Executable: "/opt/bin/devin"})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Argv[0] != "/opt/bin/devin" {
		t.Fatalf("explicit path = %q", explicit.Argv[0])
	}
	for _, model := range []string{"swe-2-high-fast", "swe-2-high-priority", "future-fast", "future-priority", "--permission-mode=dangerous", "model with spaces", "model\x00tail", "\nmodel", "model\t"} {
		if _, err := Build(Input{Harness: "devin", Model: model}); err == nil {
			t.Fatalf("expected %s to be refused", model)
		}
	}
	if _, err := Build(Input{Harness: "devin", Model: "swe-2-high", Speed: "fast"}); err == nil {
		t.Fatal("expected fast speed to be refused")
	}
}

func TestClaudeCodePreservesNativeContextSuffix(t *testing.T) {
	for _, model := range []string{"opus[1m]", "sonnet[1m]", "claude-opus-future[1m]"} {
		got, err := Build(Input{Harness: "claude", Model: model, UnattendedCodingPermissions: true})
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if got.Argv[9] != model || got.Permissions != PermissionsConstrained || strings.Join(got.Argv[len(got.Argv)-2:], " ") != "--permission-mode auto" || contains(got.Argv, "--dangerously-skip-permissions") {
			t.Fatalf("model or permissions changed: %#v", got)
		}
		if _, err := Build(Input{Harness: "cursor", Model: model}); err == nil {
			t.Fatalf("Claude native suffix accepted by Cursor: %s", model)
		}
	}
	for _, model := range []string{"[1m]", "opus[2m]", "opus[1m][1m]", "opus[[1m]", "opus[1m]suffix", "opus[1m,fast=true]"} {
		if _, err := Build(Input{Harness: "claude-code", Model: model}); err == nil {
			t.Fatalf("malformed suffix accepted: %s", model)
		}
	}
}

func TestClaudeCodeCodingPermissionGrant(t *testing.T) {
	for _, harness := range []string{"claude", "claude-code"} {
		for _, access := range []string{"", AccessCoding} {
			for _, grant := range []bool{false, true} {
				got, err := Build(Input{Harness: harness, Model: "opus[1m]", Effort: "high", Access: access, UnattendedCodingPermissions: grant})
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"claude", "--print", "--verbose", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages", "--model", "opus[1m]", "--effort", "high"}
				permissions := PermissionsConstrained
				if grant {
					want = append(want, "--permission-mode", "auto")
				}
				if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") || got.Permissions != permissions || got.PromptDelivery != PromptDeliveryStream {
					t.Fatalf("harness=%s access=%q grant=%v recipe=%#v", harness, access, grant, got)
				}
			}
		}
		for _, grant := range []bool{false, true} {
			_, err := Build(Input{Harness: harness, Model: "opus", Access: AccessReadOnly, UnattendedCodingPermissions: grant})
			var buildErr *Error
			if !errors.As(err, &buildErr) || buildErr.Code != "access_unavailable" || !strings.Contains(buildErr.Diagnostic, "edits and shell writes in --print") {
				t.Fatalf("grant=%v err=%v want explicit read_only refusal", grant, err)
			}
		}
	}
}

func TestOpenCodeRecipeArgv(t *testing.T) {
	got, err := Build(Input{Harness: "opencode", Model: "zai/glm-5.3"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"opencode", "run", "--format", "json", "--model", "zai/glm-5.3", "--"}
	if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%v want=%v", got.Argv, want)
	}
	if got.PromptDelivery != PromptDeliveryArgv {
		t.Fatalf("prompt delivery=%q want argv", got.PromptDelivery)
	}
	for _, arg := range got.Argv {
		if strings.Contains(arg, "review this") {
			t.Fatalf("prompt bytes leaked into argv: %v", got.Argv)
		}
	}
	explicit, err := Build(Input{Harness: "opencode", Model: "openai/gpt-5", Executable: "/opt/bin/opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Argv[0] != "/opt/bin/opencode" {
		t.Fatalf("explicit path = %q", explicit.Argv[0])
	}
}

func TestOpenCodeRecipeRejectsSpeedEffortAndBadModels(t *testing.T) {
	var buildErr *Error
	if _, err := Build(Input{Harness: "opencode", Model: "zai/glm-5.3", Speed: "regular"}); !errors.As(err, &buildErr) || buildErr.Code != "speed_unavailable" {
		t.Fatalf("speed err=%v want speed_unavailable", err)
	}
	if _, err := Build(Input{Harness: "opencode", Model: "zai/glm-5.3", Speed: "fast"}); !errors.As(err, &buildErr) || buildErr.Code != "speed_unavailable" {
		t.Fatalf("fast err=%v want speed_unavailable", err)
	}
	if _, err := Build(Input{Harness: "opencode", Model: "zai/glm-5.3", Effort: "high"}); !errors.As(err, &buildErr) || buildErr.Code != "effort_unavailable" {
		t.Fatalf("effort err=%v want effort_unavailable", err)
	}
	for _, model := range []string{"glm-5.3", "zai/", "/glm-5.3", "zai/glm-5.3[effort=high]", "zai /glm-5.3"} {
		if _, err := Build(Input{Harness: "opencode", Model: model}); err == nil {
			t.Fatalf("accepted %q", model)
		}
	}
	if _, err := Build(Input{Harness: "opencode"}); err == nil {
		t.Fatal("empty model was accepted")
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

func TestCursorFullPermissionsAppendsForce(t *testing.T) {
	got, err := Build(Input{
		Harness:                     "cursor",
		Model:                       "grok-4.7-high",
		CursorWorkspaceTrust:        true,
		UnattendedCodingPermissions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Permissions != PermissionsFull || !contains(got.Argv, "--force") || !contains(got.Argv, "--trust") || contains(got.Argv, "--mode") {
		t.Fatalf("argv=%v permissions=%s", got.Argv, got.Permissions)
	}
}

func TestCursorReadOnlyOmitsForce(t *testing.T) {
	got, err := Build(Input{
		Harness:                     "cursor",
		Model:                       "grok-4.7-high",
		Access:                      AccessReadOnly,
		CursorWorkspaceTrust:        true,
		UnattendedCodingPermissions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Permissions != PermissionsReadOnly || contains(got.Argv, "--force") || !contains(got.Argv, "ask") || !contains(got.Argv, "--trust") {
		t.Fatalf("argv=%v permissions=%s", got.Argv, got.Permissions)
	}
}

func TestCursorGrantAbsentStaysConstrained(t *testing.T) {
	got, err := Build(Input{Harness: "cursor", Model: "grok-4.7-high", CursorWorkspaceTrust: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Permissions != PermissionsConstrained || contains(got.Argv, "--force") {
		t.Fatalf("argv=%v permissions=%s", got.Argv, got.Permissions)
	}
}

func TestCodexFullPermissionsUsesReviewedBypass(t *testing.T) {
	got, err := Build(Input{Harness: "codex", Model: "gpt-6-astra", UnattendedCodingPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Permissions != PermissionsFull || !contains(got.Argv, "--dangerously-bypass-approvals-and-sandbox") || contains(got.Argv, "--sandbox") {
		t.Fatalf("argv=%v permissions=%s", got.Argv, got.Permissions)
	}
	if got.Argv[len(got.Argv)-1] != "-" {
		t.Fatalf("stdin sentinel moved: %v", got.Argv)
	}
}

func TestCodexReadOnlyUsesSandbox(t *testing.T) {
	got, err := Build(Input{Harness: "codex", Model: "gpt-6-astra", Access: AccessReadOnly, UnattendedCodingPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Permissions != PermissionsReadOnly || !contains(got.Argv, "read-only") || contains(got.Argv, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("argv=%v permissions=%s", got.Argv, got.Permissions)
	}
}

func TestDevinFullPermissionsUsesACPGrant(t *testing.T) {
	got, err := Build(Input{Harness: "devin", Model: "fusion-gpt-6.1-sol-high-sidekick-swe-2-high", UnattendedCodingPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"devin", "acp", "--model", "fusion-gpt-6.1-sol-high-sidekick-swe-2-high"}
	if !contains(got.Env, "AGENTCTL_DEVIN_ACP_APPROVE=1") || got.Permissions != PermissionsFull || strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%v permissions=%s", got.Argv, got.Permissions)
	}
}

func TestDevinRejectsReadOnlyAccess(t *testing.T) {
	_, err := Build(Input{Harness: "devin", Model: "swe-2-high", Access: AccessReadOnly, UnattendedCodingPermissions: true})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "access_unavailable" {
		t.Fatalf("err=%v want access_unavailable", err)
	}
}

func TestDevinGrantAbsentStaysConstrained(t *testing.T) {
	got, err := Build(Input{Harness: "devin", Model: "swe-2-high"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Permissions != PermissionsConstrained || contains(got.Argv, "--permission-mode") || contains(got.Argv, "dangerous") {
		t.Fatalf("argv=%v permissions=%s", got.Argv, got.Permissions)
	}
}

func TestOMPRejectsReadOnlyAccess(t *testing.T) {
	_, err := Build(Input{Harness: "omp", Model: "zai/glm-5.3", Access: AccessReadOnly})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "access_unavailable" {
		t.Fatalf("err=%v want access_unavailable", err)
	}
}

func TestClaudeCodeRejectsReadOnlyAccess(t *testing.T) {
	_, err := Build(Input{Harness: "claude-code", Model: "claude-sonnet-4", Access: AccessReadOnly})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "access_unavailable" {
		t.Fatalf("err=%v want access_unavailable", err)
	}
}

func TestOMPDoesNotInferBypassWhenGrantIsOn(t *testing.T) {
	got, err := Build(Input{Harness: "omp", Model: "zai/glm-5.3", UnattendedCodingPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Permissions != PermissionsUnsupported || contains(got.Argv, "--force") || contains(got.Argv, "--yolo") {
		t.Fatalf("argv=%v permissions=%s", got.Argv, got.Permissions)
	}
}

func TestRejectUnknownAccess(t *testing.T) {
	_, err := Build(Input{Harness: "cursor", Model: "grok-4.7-high", Access: "yolo"})
	var buildErr *Error
	if !errors.As(err, &buildErr) || buildErr.Code != "unsupported_access" {
		t.Fatalf("err=%v want unsupported_access", err)
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

func TestOpenCodeRecipePreservesPermissionBoundary(t *testing.T) {
	if _, err := Build(Input{Harness: "opencode", Model: "provider/model", Access: AccessReadOnly}); err == nil {
		t.Fatal("OpenCode recipe cannot guarantee read-only access")
	}
	recipe, err := Build(Input{Harness: "opencode", Model: "provider/model", UnattendedCodingPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	if recipe.Permissions != PermissionsUnsupported || strings.Join(recipe.Argv, " ") != "opencode run --format json --model provider/model --" {
		t.Fatalf("unexpected permission grant: %#v", recipe)
	}
}

func TestCursorReadOnlyRequiresWorkspaceTrust(t *testing.T) {
	for _, grant := range []bool{false, true} {
		_, err := Build(Input{Harness: "cursor", Model: "grok-4.7-high", Access: AccessReadOnly, UnattendedCodingPermissions: grant})
		var buildErr *Error
		if !errors.As(err, &buildErr) || buildErr.Code != "workspace_trust_required" || !strings.Contains(buildErr.Diagnostic, "delegation.cursor_workspace_trust") {
			t.Fatalf("grant=%v err=%v", grant, err)
		}
	}
}
func TestZCodeYoloLabelIsConstrainedRegardlessOfGrant(t *testing.T) {
	for _, grant := range []bool{false, true} {
		recipe, err := Build(Input{Harness: "zcode", Model: "zai/glm-5.3", UnattendedCodingPermissions: grant})
		if err != nil || recipe.Permissions != PermissionsConstrained || !contains(recipe.Argv, "yolo") {
			t.Fatalf("recipe=%#v err=%v", recipe, err)
		}
	}
}

func TestZCodeReadOnlyUsesPlanModeDenylistAndProfile(t *testing.T) {
	for _, grant := range []bool{false, true} {
		got, err := Build(Input{Harness: "zcode", Model: "zai/glm-5.3-flash", Access: AccessReadOnly, UnattendedCodingPermissions: grant})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"zcode", "--json", "--mode", "plan", "--disallowed-tools=" + strings.Join(ZCodeReadOnlyDisallowedTools, ","), "--prompt"}
		if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") || contains(got.Argv, "yolo") {
			t.Fatalf("grant=%v argv=%#v", grant, got.Argv)
		}
		if got.Permissions != PermissionsReadOnly || got.PromptDelivery != PromptDeliveryArgv {
			t.Fatalf("grant=%v recipe=%#v", grant, got)
		}
		if strings.Join(got.Env, " ") != "ZCODE_MODEL=glm-5.3-flash "+ZCodeReadOnlyProfileEnv {
			t.Fatalf("grant=%v env=%v", grant, got.Env)
		}
	}
	for _, tool := range []string{"Bash", "Edit", "Write", "Agent", "ExitPlanMode", "WebFetch", "mcp__web_reader__webReader"} {
		if !contains(ZCodeReadOnlyDisallowedTools, tool) {
			t.Fatalf("denylist lacks %s", tool)
		}
	}
	coding, err := Build(Input{Harness: "zcode", Model: "zai/glm-5.3"})
	if err != nil || contains(coding.Env, ZCodeReadOnlyProfileEnv) || !contains(coding.Argv, "yolo") {
		t.Fatalf("coding recipe=%#v err=%v", coding, err)
	}
}
