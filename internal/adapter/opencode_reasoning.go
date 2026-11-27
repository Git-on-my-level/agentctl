package adapter

import (
	"crypto/sha256"
	"strings"
	"unicode"
)

const (
	openCodeReasoningFallbackSource = "assistant_reasoning_fallback"
	deepSeekThinkingEnd             = "</think>"
	openCodeReasoningFrameLimit     = 64
)

type openCodeStepID struct{ session, message string }

func (id openCodeStepID) known() bool {
	return id.session != "" && id.message != ""
}

// DeepSeek can put the final answer in the same part as its reasoning, after
// the model's closing thinking marker. Keep a possible split marker and the
// bounded answer suffix only. Reconciliation uses a different ID from the live
// renderer and does not expose their shared ordinal on both frames. Only join
// fragments within the same observed part ID.
type openCodeReasoning struct {
	partID    string
	seen      map[[sha256.Size]byte]struct{}
	pending   string
	found     bool
	answer    strings.Builder
	truncated bool
	ambiguous bool
}

func (r *openCodeReasoning) append(partID, text string) {
	if partID == "" || text == "" || r.ambiguous {
		return
	}
	if partID != r.partID {
		if r.found {
			// A distinct unmarked part is thinking. Two marked parts leave
			// the source of the final answer ambiguous.
			r.ambiguous = strings.Contains(text, deepSeekThinkingEnd)
			return
		}
		*r = openCodeReasoning{partID: partID, seen: make(map[[sha256.Size]byte]struct{})}
	}
	if r.truncated {
		return
	}
	digest := sha256.Sum256([]byte(text))
	if _, duplicate := r.seen[digest]; duplicate {
		return
	}
	if len(r.seen) == openCodeReasoningFrameLimit {
		r.ambiguous = true
		return
	}
	r.seen[digest] = struct{}{}
	if !r.found {
		text = r.pending + text
		_, suffix, found := strings.Cut(text, deepSeekThinkingEnd)
		if !found {
			// Clone the short tail so it cannot retain the native reasoning frame.
			r.pending = strings.Clone(text[max(0, len(text)-len(deepSeekThinkingEnd)+1):])
			return
		}
		r.found, r.pending, text = true, "", suffix
	}
	if r.answer.Len() == 0 {
		text = strings.TrimLeftFunc(text, unicode.IsSpace)
	}
	prefix := opencodeAnswerPrefix(text, opencodeAnswerLimit-r.answer.Len())
	r.answer.WriteString(prefix)
	r.truncated = len(prefix) != len(text)
}

func (r *openCodeReasoning) finalAnswer() string {
	answer := strings.TrimSpace(r.answer.String())
	if r.ambiguous || strings.HasPrefix(answer, "<think>") || strings.HasPrefix(answer, deepSeekThinkingEnd) {
		return ""
	}
	if strings.IndexFunc(answer, func(c rune) bool {
		return unicode.IsLetter(c) || unicode.IsNumber(c) || unicode.IsSymbol(c)
	}) < 0 {
		return ""
	}
	return answer
}
