package anxrun

import (
	"math"
	"regexp"
	"strings"
)

// Diagnostic values never contain the rejected value or its field name.
const (
	ReasonEgressRejected = "egress_rejected"
	EgressSecretShape    = 1001
	EgressLength         = 1002
	EgressFormat         = 1003
)

var tokenPrefix = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:gh[opsur]_|github_pat_|glpat-|gldt-|glrt-|sk-|rk_live_|sk_live_|xox[abpr]-|hf_|npm_|pypi-|ya29\.|-----BEGIN)|AKIA|AIza`)
var jwtShape = regexp.MustCompile(`[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
var entropyRun = regexp.MustCompile(`[A-Za-z0-9+/=_-]{24,}`)

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
			counts := map[byte]int{}
			for i := range run {
				counts[run[i]]++
			}
			entropy := 0.0
			for _, count := range counts {
				p := float64(count) / float64(len(run))
				entropy -= p * math.Log2(p)
			}
			if entropy >= 3.5 {
				return EgressSecretShape
			}
		}
	}
	return 0
}

// Real word IDs contain only dictionary words plus a type-bound checksum.
// Check the full ID for prefixes/length and each word for token entropy. A
// hyphenated sentence must not make dictionary IDs look like random tokens.
func wordIDCode(value string) int {
	if code := stringCode(value, 128, true); code != 0 {
		return code
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
	case EgressSecretShape, EgressLength, EgressFormat:
		return code
	}
	return 0
}

func egressError(code int) error {
	return &SendError{Fatal: true, Reason: ReasonEgressRejected, NumericCode: code}
}
