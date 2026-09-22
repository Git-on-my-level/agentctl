package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestOperationDiagnosticNeverRetainsUpstreamText(t *testing.T) {
	for _, tc := range []struct {
		text, category   string
		retry, uncertain bool
	}{
		{"unknown flag: --description-stdin secret=private-prompt", "invalid_arguments", false, true},
		{"not authenticated: token=private-prompt", "authentication_required", false, true},
		{"permission denied private-prompt", "authorization_denied", false, true},
		{"network timeout private-prompt", "timeout", true, true},
		{"private-prompt arbitrary upstream error", "upstream_failure", true, true},
	} {
		d := classifyOperation(context.Background(), errors.New("failed"), tc.text)
		b, _ := json.Marshal(d)
		if d.Category != tc.category || d.Retryable != tc.retry || d.RemoteCreationUncertain != tc.uncertain || strings.Contains(string(b), "private-prompt") {
			t.Fatalf("diagnostic %s", b)
		}
	}
}
