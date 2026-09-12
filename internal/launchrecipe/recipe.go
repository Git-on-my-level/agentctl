package launchrecipe

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	PromptDeliveryArgv  = "argv"
	PromptDeliveryStdin = "stdin"
)

// Input carries the resolved harness tuple. Prompt text is supplied separately
// through agentctl's prompt channel; it is never embedded in Recipe.Argv.
type Input struct {
	Harness              string
	Model                string
	Speed                string
	Effort               string
	Executable           string
	CursorWorkspaceTrust bool
}

// Recipe is the exact native invocation plan before prompt attachment.
type Recipe struct {
	Argv           []string
	PromptDelivery string
}

// Error is a typed build failure with a stable diagnostic for callers.
type Error struct {
	Code       string
	Diagnostic string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Diagnostic
}

// Build returns a reviewed argv vector and prompt delivery mode for the harness.
func Build(in Input) (Recipe, error) {
	harness, err := CanonicalHarness(in.Harness)
	if err != nil {
		return Recipe{}, err
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return Recipe{}, fail("missing_model", "model is required")
	}
	if err := validateModelBrackets(model); err != nil {
		return Recipe{}, err
	}
	speed, err := normalizeSpeed(in.Speed)
	if err != nil {
		return Recipe{}, err
	}
	effort, err := normalizeEffort(in.Effort)
	if err != nil {
		return Recipe{}, err
	}
	if err := validateModelSpeed(model, speed); err != nil && harness == "cursor" {
		return Recipe{}, err
	}
	if err := rejectForbiddenArgvTokens(model); err != nil {
		return Recipe{}, err
	}

	switch harness {
	case "cursor":
		return buildCursor(in.Executable, model, speed, effort, in.CursorWorkspaceTrust)
	case "codex":
		return buildCodex(in.Executable, model, speed, effort)
	case "omp":
		return buildOMP(in.Executable, model, speed, effort)
	case "claude-code":
		return buildClaudeCode(in.Executable, model, speed, effort)
	default:
		return Recipe{}, fail("unsupported_harness", fmt.Sprintf("harness %q is not supported by launch recipes", in.Harness))
	}
}

func fail(code, diagnostic string) *Error {
	return &Error{Code: code, Diagnostic: diagnostic}
}

func CanonicalHarness(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	switch h {
	case "cursor", "cursor-agent":
		return "cursor", nil
	case "codex":
		return "codex", nil
	case "omp", "oh-my-pi":
		return "omp", nil
	case "claude", "claude-code":
		return "claude-code", nil
	case "":
		return "", fail("missing_harness", "harness is required")
	default:
		return "", fail("unsupported_harness", fmt.Sprintf("harness %q is not supported by launch recipes", raw))
	}
}

func normalizeSpeed(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "", "regular", "fast":
		return s, nil
	default:
		return "", fail("unsupported_speed", fmt.Sprintf("speed %q is not a supported service-speed guarantee", raw))
	}
}

var commonEfforts = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {},
}

var codexEfforts = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {}, "ultra": {},
}

func normalizeEffort(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if e == "" {
		return "", nil
	}
	if _, ok := commonEfforts[e]; ok {
		return e, nil
	}
	if _, ok := codexEfforts[e]; ok {
		return e, nil
	}
	return "", fail("unsupported_effort", fmt.Sprintf("effort %q is not a supported reasoning-effort guarantee", raw))
}

func defaultExecutable(harness string) string {
	switch harness {
	case "cursor":
		return "cursor-agent"
	case "codex":
		return "codex"
	case "omp":
		return "omp"
	case "claude-code":
		return "claude"
	default:
		return ""
	}
}

func resolveExecutable(harness, override string) (string, error) {
	exe := override
	if exe == "" {
		exe = defaultExecutable(harness)
	}
	if strings.TrimSpace(exe) == "" {
		return "", fail("missing_executable", "executable is required")
	}
	return exe, nil
}

func validateModelSpeed(model, speed string) error {
	fastVariant := isFastModelVariant(model)
	switch speed {
	case "regular":
		if fastVariant {
			return fail("speed_model_mismatch", "regular service speed cannot use a fast model variant")
		}
	case "fast":
		if !fastVariant && !canExpressFastInBrackets(model) {
			return fail("speed_unavailable", "fast service speed cannot be guaranteed for this model id")
		}
	}
	return nil
}

func isFastModelVariant(model string) bool {
	base, bracket := splitModelBrackets(model)
	if strings.HasSuffix(base, "-fast") {
		return true
	}
	for _, part := range parseBracketParams(bracket) {
		if part.key == "fast" && strings.EqualFold(part.value, "true") {
			return true
		}
	}
	return false
}

func canExpressFastInBrackets(model string) bool {
	_, bracket := splitModelBrackets(model)
	if bracket == "" {
		return cursorBracketCapable(model)
	}
	for _, part := range parseBracketParams(bracket) {
		if part.key == "fast" {
			return true
		}
	}
	return cursorBracketCapable(model)
}

func rejectForbiddenArgvTokens(model string) error {
	lower := strings.ToLower(model)
	forbidden := []string{
		"--yolo", "--force", "--auto-approve", "--approve-mcps",
		"--dangerously-bypass-approvals-and-sandbox", "--no-sandbox",
		"danger-full-access",
	}
	for _, token := range forbidden {
		if strings.Contains(lower, token) {
			return fail("forbidden_permission", fmt.Sprintf("model argument cannot contain permission-broadening token %q", token))
		}
	}
	return nil
}

type bracketParam struct {
	key   string
	value string
}

func splitModelBrackets(model string) (string, string) {
	open := strings.Index(model, "[")
	if open < 0 {
		return model, ""
	}
	close := strings.LastIndex(model, "]")
	if close < open {
		return model, ""
	}
	return model[:open], model[open+1 : close]
}

func parseBracketParams(bracket string) []bracketParam {
	if strings.TrimSpace(bracket) == "" {
		return nil
	}
	var out []bracketParam
	for _, piece := range strings.Split(bracket, ",") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		key, value, ok := strings.Cut(piece, "=")
		if !ok {
			continue
		}
		out = append(out, bracketParam{key: strings.TrimSpace(key), value: strings.TrimSpace(value)})
	}
	return out
}

func mergeBracketParams(bracket string, set map[string]string) (string, *Error) {
	existing := map[string]string{}
	for _, part := range parseBracketParams(bracket) {
		existing[part.key] = part.value
	}
	for key, want := range set {
		if have, ok := existing[key]; ok && !strings.EqualFold(have, want) {
			return "", fail("bracket_conflict", fmt.Sprintf("model bracket already sets %s=%s", key, have))
		}
		existing[key] = want
	}
	if len(existing) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(existing))
	for key := range existing {
		keys = append(keys, key)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+existing[key])
	}
	return strings.Join(parts, ","), nil
}

func applyBracketModel(base, bracket string) string {
	if bracket == "" {
		return base
	}
	return base + "[" + bracket + "]"
}

func cursorBracketCapable(model string) bool {
	base, _ := splitModelBrackets(model)
	return strings.HasPrefix(base, "claude-")
}

func codexConfigOverride(key, value string) string {
	// Codex exec -c accepts TOML assignment strings; Quote yields a safe argv element.
	return key + "=" + strconv.Quote(value)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

// Reject malformed parameters instead of silently dropping parts of an exact ID.
func validateModelBrackets(model string) error {
	if !strings.ContainsAny(model, "[]") {
		return nil
	}
	open := strings.IndexByte(model, '[')
	if open <= 0 || !strings.HasSuffix(model, "]") || strings.Count(model, "[") != 1 || strings.Count(model, "]") != 1 {
		return fail("invalid_model_parameters", "model parameters must be one final bracket block")
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(model[open+1:len(model)-1], ",") {
		key, value, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.TrimSpace(value) == "" || seen[key] {
			return fail("invalid_model_parameters", "model parameters must be nonempty unique assignments")
		}
		seen[key] = true
	}
	return nil
}
