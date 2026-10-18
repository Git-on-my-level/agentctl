package adapter

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	diagnosticJSONValue  = regexp.MustCompile(`(?i)("(?:api[_-]?key|authorization|password|(?:client[_-]?)?secret|(?:(?:access|refresh|id)[_-]?)?token|(?:set[_-]?)?cookie|prompt|input|messages?|request[_ -]?id|trace[_ -]?id)"\s*:\s*)("(?:\\.|[^"\\])*"|\{.*|\[.*|[^,}\s]+)`)
	diagnosticAssignment = regexp.MustCompile(`(?i)\b(api[_-]?key|authorization|password|(?:client[_-]?)?secret|(?:(?:access|refresh|id)[_-]?)?token|(?:set[_-]?)?cookie|prompt|input|messages?|request[_ -]?id|trace[_ -]?id)(\s*[=:]\s*|\s+)("[^"]*"|'[^']*'|\S+)`)
	diagnosticInputTail  = regexp.MustCompile(`(?i)\b(prompt|input|messages?)\s*[=:].*`)
	diagnosticBearer     = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	diagnosticURL        = regexp.MustCompile(`(?i)https?://\S+`)
	diagnosticKey        = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`)
	diagnosticANSI       = regexp.MustCompile("\\x1b\\[[0-?]*[ -/]*[@-~]")
)

// RedactFailureLine returns only the first nonempty diagnostic line, stripped
// of common credentials, echoed input fields and request identifiers. It is a
// diagnostic fallback, never an answer or an unbounded stderr transcript.
func RedactFailureLine(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = diagnosticANSI.ReplaceAllString(line, "")
		line = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, line)
		if strings.TrimSpace(line) == "" {
			continue
		}
		line = diagnosticJSONValue.ReplaceAllString(line, `${1}"[REDACTED]"`)
		line = diagnosticInputTail.ReplaceAllString(line, "$1=[REDACTED]")
		line = diagnosticBearer.ReplaceAllString(line, "bearer [REDACTED]")
		line = diagnosticAssignment.ReplaceAllString(line, "$1=[REDACTED]")
		line = diagnosticURL.ReplaceAllString(line, "[REDACTED_URL]")
		line = diagnosticKey.ReplaceAllString(line, "[REDACTED]")
		return boundedUTF8(strings.Join(strings.Fields(line), " "), 200)
	}
	return ""
}
