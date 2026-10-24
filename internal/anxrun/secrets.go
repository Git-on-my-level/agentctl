package anxrun

import (
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

// Diagnostic values never contain the rejected value or its field name.
const (
	ReasonEgressRejected = "egress_rejected"
	EgressSecretShape    = 1001
	EgressLength         = 1002
	EgressFormat         = 1003
)

var tokenPrefix = regexp.MustCompile(`(?i)(?:gh[opsur]_|github_pat_|glpat-|gldt-|glrt-|sk-|rk_live_|sk_live_|xox[abpr]-|hf_|npm_|pypi-|ya29\.|-----BEGIN)|AKIA|AIza`)
var jwtShape = regexp.MustCompile(`[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
var entropyRun = regexp.MustCompile(`[A-Za-z0-9+/=_-]{24,}`)

// Disclosed paths/host coordinates have no privacy length or charset policy.
// Check credential prefixes/JWTs across the whole field, then entropy within
// each path component so joining ordinary directory names cannot invent a
// high-entropy token. An actual token in any component rejects this field.
func disclosureCode(value string) int {
	if !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
		return EgressFormat
	}
	if tokenPrefix.MatchString(value) || jwtShape.MatchString(value) {
		return EgressSecretShape
	}
	for _, token := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if code := stringCode(token, len(token), false); code != 0 {
			return code
		}
	}
	return 0
}

// DisclosureValue is for read-only diagnostic fields, never raw errors/prose.
func DisclosureValue(value string) string {
	if disclosureCode(value) != 0 {
		return "[redacted]"
	}
	return value
}

// stringCode is a second layer on every variable string's strict type/enum
// check. It is not a redactor for prose. Only validated cryptographic types
// (SHA/HMAC/fingerprint/UUID) bypass entropy checking; prefixes and bounds still
// apply. Callers must validate those types, never infer them from entropy.
func stringCode(value string, limit int, cryptographic bool) int {
	if len(value) > limit {
		return EgressLength
	}
	if tokenPrefix.MatchString(value) || jwtShape.MatchString(value) {
		return EgressSecretShape
	}
	if !cryptographic {
		for _, run := range entropyRun.FindAllString(value, -1) {
			// Examine every 24-byte window, so low-entropy padding cannot
			// dilute a token-shaped substring. Iterate counts deterministically.
			for start := 0; start+24 <= len(run); start++ {
				counts := [256]int{}
				for _, value := range []byte(run[start : start+24]) {
					counts[value]++
				}
				entropy := 0.0
				for _, count := range counts {
					if count > 0 {
						p := float64(count) / 24
						entropy -= p * math.Log2(p)
					}
				}
				if entropy >= 3.5 {
					return EgressSecretShape
				}
			}
		}
	}
	return 0
}

// Real word IDs contain only dictionary words plus a type-bound checksum.
// Validate the generator grammar first, then check each dictionary word. A
// hyphenated sentence must not make dictionary IDs look like random tokens.
func wordIDCode(value string) int {
	if _, err := ids.Parse(value); err != nil {
		if code := stringCode(value, 128, false); code != 0 {
			return code
		}
		return EgressFormat
	}
	for _, word := range strings.Split(value, "-") {
		if code := stringCode(word, 128, false); code != 0 {
			return code
		}
	}
	return 0
}

func validEgressCode(code int) int {
	switch code {
	case EgressSecretShape, EgressLength, EgressFormat, EgressUnverified:
		return code
	}
	return 0
}

func egressError(code int) error {
	return &SendError{Fatal: true, Reason: ReasonEgressRejected, NumericCode: code}
}
