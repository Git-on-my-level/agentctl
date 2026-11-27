package adapter

import (
	"strings"
	"unicode"
)

const (
	openCodeReasoningFallbackSource = "assistant_reasoning_fallback"
	deepSeekThinkingEnd             = "</think>"
)

// DeepSeek can put the final answer in the same part as its reasoning, after
// the model's closing thinking marker. Keep a possible split marker and the
// bounded answer suffix only. OpenCode may reconcile a part in multiple frames.
type openCodeReasoning struct {
	sessionID, messageID string
	pending              string
	found                bool
	answer               strings.Builder
	truncated            bool
}

func (r *openCodeReasoning) matches(obs parsedObservation) bool {
	return r.sessionID != "" && r.messageID != "" && r.sessionID == obs.SessionID && r.messageID == obs.openCodeMessageID
}

func (r *openCodeReasoning) append(text string) {
	if text == "" || r.truncated {
		return
	}
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
	if strings.Contains(answer, "<think>") || strings.Contains(answer, deepSeekThinkingEnd) {
		return ""
	}
	if strings.IndexFunc(answer, func(c rune) bool {
		return unicode.IsLetter(c) || unicode.IsNumber(c) || unicode.IsSymbol(c)
	}) < 0 {
		return ""
	}
	return answer
}
