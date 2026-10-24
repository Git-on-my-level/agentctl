// Package diagnostic maps native stderr to content-free correlation metadata.
package diagnostic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

// Stderr contains no source text. Hash is the first 96 bits of SHA-256 over the
// first nonempty scanner line (without its newline); Bytes is its full length.
type Stderr struct {
	Code  string `json:"code"`
	Hash  string `json:"hash"`
	Bytes int    `json:"bytes"`
}

const CodexCapacityMessage = "Selected model is at capacity. Please try a different model."

func IsCodexCapacity(message string) bool { return strings.TrimSpace(message) == CodexCapacityMessage }
func AuthenticationRequired(message string) bool {
	return containsAny(strings.ToLower(message), "authentication required", "authentication failed", "auth required", "auth failed", "not authenticated", "unauthorized", "login required", "please login", "please log in", "sign in", "invalid refresh token", "oauthrefreshfailureerror")
}

var hashPattern = regexp.MustCompile(`^sha256:[a-f0-9]{24}$`)

// NetworkFailure requires an explicit transport failure, not a generic provider
// error or bare HTTP status. Authentication classification must run first.
func NetworkFailure(message string) bool {
	return containsAny(strings.ToLower(message), "network connection failed", "connection refused", "connection reset", "network is unreachable", "no such host", "temporary failure in name resolution", "tls handshake timeout", "i/o timeout", "econnreset", "econnrefused", "enotfound", "eai_again", "connect etimedout", "connection timed out", "error sending request for url")
}

var messages = map[string]string{
	"unclassified":             "native execution failed",
	"provider_capacity":        "Selected model is at capacity. Please try a different model.",
	"workspace_trust_required": "workspace trust required",
	"authentication_required":  "authentication required",
	"approval_required":        "approval required",
	"quota_exceeded":           "quota or rate limit exceeded",
	"model_not_found":          "model not found",
	"executable_not_found":     "executable not found",
	"permission_denied":        "permission denied",
	"network":                  "network connection failed",
	"invalid_arguments":        "invalid native command arguments",
}

// FromStderrLine examines at most 4096 bytes for a fixed allow-listed pattern
// set. It hashes the full bounded scanner line, never retaining its text.
func FromStderrLine(line []byte, harnesses ...string) *Stderr {
	harness := ""
	if len(harnesses) > 0 {
		harness = harnesses[0]
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	sum := sha256.Sum256(line)
	prefix := line
	if len(prefix) > 4096 {
		prefix = prefix[:4096]
	}
	text := strings.ToLower(string(prefix))
	code := "unclassified"
	switch {
	case AuthenticationRequired(text):
		code = "authentication_required"
	case harness == "codex" && len(line) <= 4096 && IsCodexCapacity(string(line)):
		code = "provider_capacity"
	case strings.Contains(text, "workspace") && strings.Contains(text, "trust"):
		code = "workspace_trust_required"
	case containsAny(text, "permission denied", "operation not permitted", "access is denied"):
		code = "permission_denied"
	case containsAny(text, "model not found", "unknown model", "model does not exist"):
		code = "model_not_found"
	case containsAny(text, "command not found", "executable file not found", "no such file or directory"):
		code = "executable_not_found"
	case NetworkFailure(text):
		code = "network"
	case containsAny(text, "unknown flag", "unknown option", "unexpected argument", "unrecognized option"):
		code = "invalid_arguments"
	case containsAny(text, "approval required", "approval denied", "permission required"):
		code = "approval_required"
	case containsAny(text, "quota", "rate limit"):
		code = "quota_exceeded"
	}
	return &Stderr{Code: code, Hash: "sha256:" + hex.EncodeToString(sum[:12]), Bytes: len(line)}
}

func containsAny(text string, patterns ...string) bool {
	for _, pattern := range patterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

// Message returns only a constant allow-listed template, never caller text.
func (s *Stderr) Message() string {
	if s == nil {
		return ""
	}
	if message, ok := messages[s.Code]; ok {
		return message
	}
	return messages["unclassified"]
}

func (s *Stderr) Validate() error {
	if s == nil {
		return nil
	}
	if _, ok := messages[s.Code]; !ok {
		return errors.New("invalid stderr diagnostic code")
	}
	if !hashPattern.MatchString(s.Hash) || s.Bytes < 1 || s.Bytes > 8<<20 {
		return errors.New("invalid stderr correlation metadata")
	}
	return nil
}
