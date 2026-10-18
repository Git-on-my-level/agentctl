package diagnostic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestStderrMetadataNeverRetainsSourceText(t *testing.T) {
	for _, line := range []string{
		"fatal: unable to process verbatim confidential user prompt",
		"fatal: opaque-token-Xb7sQ9",
		"model not found: verbatim confidential user prompt opaque-token-Xb7sQ9",
		`{"error":"opaque-token-Xb7sQ9","prompt":"verbatim confidential user prompt"}`,
	} {
		got := FromStderrLine([]byte(line))
		sum := sha256.Sum256([]byte(line))
		if err := got.Validate(); err != nil {
			t.Fatal(err)
		}
		if got.Hash != "sha256:"+hex.EncodeToString(sum[:12]) || got.Bytes != len(line) {
			t.Fatalf("incorrect correlation: %#v", got)
		}
		encoded, _ := json.Marshal(got)
		for _, secret := range []string{"verbatim confidential user prompt", "opaque-token-Xb7sQ9", line} {
			if strings.Contains(string(encoded)+got.Message(), secret) {
				t.Fatal("source text escaped metadata boundary")
			}
		}
	}
}

func TestStderrClassifierIsBoundedAndAllowlisted(t *testing.T) {
	for _, tc := range []struct{ line, code string }{
		{"Selected model is at capacity", "provider_capacity"},
		{"HTTP 429", "provider_capacity"},
		{"workspace trust required", "workspace_trust_required"},
		{"Authentication required", "authentication_required"},
		{"OAuthRefreshFailureError: invalid refresh token private-value", "authentication_required"},
		{"unknown model private-model-id", "model_not_found"},
		{"command not found: private-command", "executable_not_found"},
		{"operation not permitted private-path", "permission_denied"},
		{"connection refused private-host", "network"},
		{"unexpected argument '--private-flag'", "invalid_arguments"},
		{strings.Repeat("x", 4096) + " at capacity", "unclassified"},
	} {
		got := FromStderrLine([]byte(tc.line))
		if got.Code != tc.code {
			t.Fatalf("code=%s want=%s", got.Code, tc.code)
		}
	}
	if FromStderrLine([]byte("   ")) != nil {
		t.Fatal("empty line must be skipped")
	}
	for _, invalid := range []*Stderr{
		{Code: "arbitrary source text", Hash: "sha256:" + strings.Repeat("a", 24), Bytes: 1},
		{Code: "unclassified", Hash: "arbitrary source text", Bytes: 1},
		{Code: "unclassified", Hash: "sha256:" + strings.Repeat("a", 24), Bytes: 0},
	} {
		if invalid.Validate() == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
}
