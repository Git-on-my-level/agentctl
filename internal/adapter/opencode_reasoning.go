package adapter

import (
	"crypto/sha256"
	"slices"
	"strings"
	"unicode"
)

const (
	openCodeReasoningFallbackSource = "assistant_reasoning_fallback"
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
	_, answer, marked := strings.Cut(text, deepSeekThinkingEnd)
	if !marked {
		r.ambiguous = r.answer != "" && strings.TrimSpace(text) != ""
		return
	}
	answer = strings.TrimSpace(answer)
	if strings.Contains(answer, "<think>") || strings.HasPrefix(answer, deepSeekThinkingEnd) || strings.IndexFunc(answer, answerCharacter) < 0 {
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

func (r *openCodeReasoning) finalAnswer() string {
	if r.ambiguous {
		return ""
	}
	return r.answer
}

func answerCharacter(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsNumber(c) || unicode.IsSymbol(c)
}

func openCodeThinkingEnabled(args []string) bool {
	separator := slices.Index(args, "--")
	if separator < 0 {
		return false
	}
	args = args[:separator]
	for i, arg := range args {
		switch arg {
		case "--no-thinking", "--thinking=false":
			return false
		case "--thinking=true":
			return true
		case "--thinking":
			return i+1 == len(args) || args[i+1] != "false"
		}
	}
	return false
}
