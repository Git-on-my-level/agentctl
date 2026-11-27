package adapter

import (
	"crypto/sha256"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
	"github.com/Git-on-my-level/agentctl/internal/route"
)

const (
	openCodeReasoningFallbackSource = "assistant_reasoning_fallback"
	deepSeekThinkingStart           = "<think>"
	deepSeekThinkingEnd             = "</think>"
)

type openCodeStepID struct{ session, message string }

func (id openCodeStepID) known() bool {
	return id.session != "" && id.message != ""
}

// DeepSeek can put the answer after its closing thinking marker in a completed
// reasoning part. OpenCode v2 emits one completed frame per part per renderer.
// Live and reconciled frames use different IDs and omit their shared ordinal,
// so a suffix in a later frame cannot be reliably assigned to a live part.
type openCodeReasoning struct {
	digest    [sha256.Size]byte
	answer    string
	truncated bool
	ambiguous bool
}

func (r *openCodeReasoning) observe(partID, text string) {
	if partID == "" || r.ambiguous {
		return
	}
	before, answer, marked := strings.Cut(text, deepSeekThinkingEnd)
	if !marked || !openCodeAnswerBoundary(before, answer) || protectedOpenCodeMarker(text, len(before)) {
		r.ambiguous = r.answer != "" && strings.TrimSpace(text) != ""
		return
	}
	answer = strings.TrimSpace(answer)
	// A later active marker makes it impossible to distinguish an answer
	// from planning that merely mentioned the first marker.
	if strings.IndexFunc(answer, answerCharacter) < 0 || openCodeHasActiveThinkingTag(answer) {
		r.ambiguous = r.answer != ""
		return
	}
	digest := sha256.Sum256([]byte(answer))
	if r.answer != "" {
		// Live and reconciled frames may have different IDs. Agreement on
		// the entire answer is sufficient without joining any fragments.
		r.ambiguous = r.digest != digest
		return
	}
	r.digest = digest
	prefix := opencodeAnswerPrefix(answer, opencodeAnswerLimit)
	// Copy the bounded result so it cannot retain the full reasoning frame.
	r.answer = strings.Clone(prefix)
	r.truncated = len(prefix) < len(answer)
}

func openCodeAnswerBoundary(before, after string) bool {
	// The model's closing marker is attached to the last reasoning sentence.
	// A tag mentioned as ordinary prose, separated by spaces, is ambiguous.
	last, _ := utf8.DecodeLastRuneInString(before)
	first, _ := utf8.DecodeRuneInString(after)
	return unicode.Is(unicode.Sentence_Terminal, last) &&
		(!unicode.IsSpace(first) || first == '\n' || first == '\r' || first == '\u2028' || first == '\u2029')
}

func (r *openCodeReasoning) finalAnswer() string {
	if r.ambiguous {
		return ""
	}
	return r.answer
}

func answerCharacter(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsNumber(c) || unicode.IsSymbol(c)
}

func openCodeReasoningFallbackEnabled(argv []string) bool {
	model := route.NativeArgvModel("opencode", argv)
	return launchrecipe.OpenCodeReasoningFallbackModel(model) && openCodeThinkingEnabled(argv[1:])
}

// OpenCodeLegacyRecipeArgv returns an adjusted managed recipe for a version
// older than the verified display-option floor. A nil result leaves argv
// alone, including when a wrapper reports an unrecognized version string.
func OpenCodeLegacyRecipeArgv(argv []string, backendVersion string) []string {
	if !openCodeReasoningFallbackEnabled(argv) || !openCodeBelowThinkingFloor(backendVersion) {
		return nil
	}
	separator := slices.Index(argv, "--")
	if flag := slices.Index(argv[:separator], "--thinking"); flag >= 0 {
		return slices.Delete(slices.Clone(argv), flag, flag+1)
	}
	return nil
}

func openCodeBelowThinkingFloor(backendVersion string) bool {
	version := strings.TrimSpace(backendVersion)
	version = strings.TrimPrefix(strings.TrimPrefix(version, "opencode "), "v")
	core, _, _ := strings.Cut(version, "+")
	core, _, prerelease := strings.Cut(core, "-")
	parts := strings.Split(core, ".")
	var parsed [3]int
	if len(parts) != len(parsed) {
		return false
	}
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return false
		}
		parsed[i] = value
	}
	comparison := slices.Compare(parsed[:], []int{1, 17, 13})
	return comparison < 0 || comparison == 0 && prerelease
}

func openCodeThinkingEnabled(args []string) bool {
	separator := slices.Index(args, "--")
	if separator < 0 {
		return false
	}
	args = args[:separator]
	seen, enabled := false, false
	for i, arg := range args {
		var value bool
		switch arg {
		case "--no-thinking", "--thinking=false":
		case "--thinking=true":
			value = true
		case "--thinking":
			value = i+1 == len(args) || args[i+1] != "false"
		default:
			continue
		}
		if seen {
			return false
		}
		seen, enabled = true, value
	}
	return enabled
}
