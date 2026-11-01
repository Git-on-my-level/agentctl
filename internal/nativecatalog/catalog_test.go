package nativecatalog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeDevin = `#!/bin/sh
[ "$1" = acp ] || exit 9
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*) echo 'not json'; echo '{"jsonrpc":"2.0","id":1,"result":{}}' ;;
    *'"session/new"'*) echo '{"jsonrpc":"2.0","method":"session/update","params":{}}'
      echo '{"jsonrpc":"2.0","id":2,"result":{"configOptions":[OPTIONS]}}' ;;
  esac
done
`

const devinModels = `{"id":"model","options":[{"value":"swe-2-high"},{"options":[{"value":"gemini-4-argon-medium"}]}]}`

func script(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func one(t *testing.T, entry Entry) Check {
	t.Helper()
	report := CheckEntries(context.Background(), []Entry{entry}, nil)
	if len(report.Checks) != 1 {
		t.Fatalf("checks = %+v", report.Checks)
	}
	return report.Checks[0]
}

func TestDevinUsesACPOptionsAndThoughtLevel(t *testing.T) {
	current := script(t, "devin", strings.ReplaceAll(fakeDevin, "OPTIONS", devinModels+`,{"id":"thought_level","options":[{"value":"medium"},{"value":"high"}]}`))
	for _, test := range []struct {
		model, effort, status, code string
	}{
		{"swe-2-high", "", StatusAvailable, ""},
		{"gemini-4-argon-medium", "high", StatusAvailable, ""},
		{"gemini-4-argon-medium", "max", StatusUnavailable, "effort_unavailable"},
		{"gemini-4-argon-high", "", StatusUnavailable, "model_unavailable"},
	} {
		got := one(t, Entry{Harness: "devin", Model: test.model, Effort: test.effort, Executable: current})
		if got.Status != test.status || got.DiagnosticCode != test.code {
			t.Errorf("%s/%s = %+v", test.model, test.effort, got)
		}
	}
}

// An older Devin CLI has no thought_level option, so a configured effort
// cannot be applied even though the model exists.
func TestDevinWithoutThoughtLevelRefusesEffort(t *testing.T) {
	old := script(t, "devin", strings.ReplaceAll(fakeDevin, "OPTIONS", devinModels))
	got := one(t, Entry{Harness: "devin", Model: "gemini-4-argon-medium", Effort: "high", Executable: old})
	if got.Status != StatusUnavailable || got.DiagnosticCode != "effort_unavailable" {
		t.Fatalf("got %+v", got)
	}
	if got := one(t, Entry{Harness: "devin", Model: "swe-2-high", Executable: old}); got.Status != StatusAvailable {
		t.Fatalf("model without effort = %+v", got)
	}
}

func TestDevinThatNeverAnswersIsUnverified(t *testing.T) {
	silent := script(t, "devin", "#!/bin/sh\nexit 0\n")
	got := one(t, Entry{Harness: "devin", Model: "swe-2-high", Executable: silent})
	if got.Status != StatusUnverified || got.DiagnosticCode != "catalog_probe_failed" {
		t.Fatalf("got %+v", got)
	}
}

func TestCodexComparesModelAndEffort(t *testing.T) {
	codex := script(t, "codex", `#!/bin/sh
[ "$1 $2" = "debug models" ] || exit 9
echo '{"models":[{"slug":"gpt-6-luna","supported_reasoning_levels":[{"effort":"xhigh"}]}]}'
`)
	for _, test := range []struct {
		model, effort, status string
	}{
		{"gpt-6-luna", "xhigh", StatusAvailable},
		{"gpt-6-luna", "ultra", StatusUnavailable},
		{"gpt-5", "", StatusUnavailable},
	} {
		if got := one(t, Entry{Harness: "codex", Model: test.model, Effort: test.effort, Executable: codex}); got.Status != test.status {
			t.Errorf("%s/%s = %+v", test.model, test.effort, got)
		}
	}
}

func TestCursorParsesModelLines(t *testing.T) {
	cursor := script(t, "cursor-agent", `#!/bin/sh
[ "$1" = --list-models ] || exit 9
printf 'Available models\n\nauto - Auto (default)\ngrok-4.7-high - Grok 4.7  High\n'
`)
	if got := one(t, Entry{Harness: "cursor", Model: "grok-4.7-high", Executable: cursor}); got.Status != StatusAvailable {
		t.Fatalf("got %+v", got)
	}
	if got := one(t, Entry{Harness: "cursor", Model: "Available", Executable: cursor}); got.Status != StatusUnavailable {
		t.Fatalf("heading treated as a model: %+v", got)
	}
}

func TestProbeFailureHidesNativeOutput(t *testing.T) {
	cursor := script(t, "cursor-agent", "#!/bin/sh\necho account-secret\necho account-secret >&2\nexit 3\n")
	got := one(t, Entry{Harness: "cursor", Model: "composer-2.5", Executable: cursor})
	if got.Status != StatusUnverified || got.DiagnosticCode != "catalog_probe_failed" || strings.Contains(got.Diagnostic, "secret") || !strings.Contains(got.Diagnostic, "exit code 3") {
		t.Fatalf("got %+v", got)
	}
}

func TestStatusSummary(t *testing.T) {
	present := script(t, "claude", "#!/bin/sh\nexit 9\n")
	if got := one(t, Entry{Harness: "claude-code", Model: "x", Executable: present}); got.Status != StatusUnverified || got.DiagnosticCode != "catalog_unsupported" {
		t.Fatalf("claude = %+v", got)
	}
	if got := one(t, Entry{Harness: "devin-cloud", Model: "x", Executable: present}); got.Status != StatusUnverified {
		t.Fatalf("cloud must not be probed: %+v", got)
	}
	missing := Entry{Harness: "codex", Model: "x", Executable: filepath.Join(t.TempDir(), "absent")}
	if got := one(t, missing); got.Status != StatusUnavailable || got.DiagnosticCode != "adapter_missing" {
		t.Fatalf("missing = %+v", got)
	}
	for _, test := range []struct {
		statuses []string
		want     string
	}{
		{nil, StatusEmpty},
		{[]string{StatusAvailable}, StatusAvailable},
		{[]string{StatusAvailable, StatusUnverified}, StatusPartial},
		{[]string{StatusUnverified, StatusUnavailable}, StatusUnavailable},
	} {
		var checks []Check
		for _, status := range test.statuses {
			checks = append(checks, Check{Status: status})
		}
		if got := summarize(checks); got != test.want {
			t.Errorf("%v = %s", test.statuses, got)
		}
	}
}
