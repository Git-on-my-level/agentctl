package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/delegation"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func decodeDelegateDiagnostic(t *testing.T, body string) *output.Error {
	t.Helper()
	var document output.ErrorDocument
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		t.Fatalf("decode diagnostic: %v: %s", err, body)
	}
	if document.OK || document.Error == nil {
		t.Fatalf("not error: %s", body)
	}
	return document.Error
}

func assertDelegateNoExecutionSideEffects(t *testing.T, f delegateFixture) {
	t.Helper()
	for _, path := range []string{f.count, f.probe, f.journal} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("rejection touched %s: %v", path, err)
		}
	}
}

func TestDelegateUnknownFieldDiagnosticIsActionableAndPrivate(t *testing.T) {
	for _, authority := range []string{"native", "multica"} {
		t.Run(authority, func(t *testing.T) {
			f := newDelegateFixture(t, delegateSuccessScript)
			f.selectRequest(t, `{"family":"private-family","settings":{"sped":"private-value"}}`)
			code, out := f.invoke("private-prompt", "--plan", "--authority", authority)
			if code != output.ExitCodeFor(output.CodeUsage) {
				t.Fatalf("exit=%d %s", code, out)
			}
			issue := decodeDelegateDiagnostic(t, out)
			if issue.Details["diagnostic_code"] != "delegate_invalid_request" || issue.Details["field_path"] != "$.selector.settings.sped" || issue.Details["violation"] != "unknown_field" {
				t.Fatalf("issue=%#v", issue)
			}
			if !reflect.DeepEqual(issue.Details["allowed_keys"], []any{"access", "auto_compact_window", "effort", "speed"}) {
				t.Fatalf("keys=%#v", issue.Details["allowed_keys"])
			}
			example, _ := json.Marshal(issue.Details["example_request"])
			if _, err := delegation.DecodeRequest(example); err != nil {
				t.Fatalf("invalid example: %s %v", example, err)
			}
			if issue.Details["example_guidance"] == nil || strings.Contains(out, "private-") {
				t.Fatalf("unsafe/unhelpful error: %s", out)
			}
			assertDelegateNoExecutionSideEffects(t, f)
		})
	}
}

func TestDelegateNoMatchingTupleDiagnosticPreservesAuthorityAndConstraints(t *testing.T) {
	for _, authority := range []string{"native", "multica"} {
		t.Run(authority, func(t *testing.T) {
			f := newDelegateFixture(t, delegateSuccessScript)
			f.selectRequest(t, `{"family":"private-family","settings":{"speed":"private-speed"}}`)
			code, out := f.invoke("private-prompt", "--plan", "--authority", authority)
			if code != output.ExitCodeFor(output.CodeUsage) {
				t.Fatalf("exit=%d %s", code, out)
			}
			issue := decodeDelegateDiagnostic(t, out)
			if issue.Details["diagnostic_code"] != "delegate_no_matching_tuple" || issue.Details["candidate_count"] != float64(1) || issue.Details["candidates_truncated"] != false {
				t.Fatalf("issue=%#v", issue)
			}
			if !reflect.DeepEqual(issue.Details["constraint_fields"], []any{"family", "settings.speed"}) {
				t.Fatalf("constraints=%#v", issue.Details["constraint_fields"])
			}
			selectors, _ := json.Marshal(issue.Details["candidate_selectors"])
			var candidates []delegation.Selector
			if err := json.Unmarshal(selectors, &candidates); err != nil || len(candidates) != 1 || candidates[0].Model != "cursor-grok-4.6-high" || candidates[0].Settings.Speed != "regular" {
				t.Fatalf("selectors=%s err=%v", selectors, err)
			}
			if strings.Contains(out, "private-") || !strings.Contains(issue.Details["selection_guidance"].(string), "preserve explicit user constraints") {
				t.Fatalf("unsafe/unhelpful error: %s", out)
			}
			if len(issue.NextActions) != 1 || !reflect.DeepEqual(issue.NextActions[0].Argv, []string{"agentctl", "help", "route"}) || issue.NextActions[0].SideEffectClass != output.ReadOnly {
				t.Fatalf("actions=%#v", issue.NextActions)
			}
			assertDelegateNoExecutionSideEffects(t, f)
		})
	}
}

func TestDelegateValidMulticaSelectionStillFailsExplicitCapabilityGate(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	code, out := f.invoke("private-prompt", "--plan", "--authority", "multica")
	issue := decodeDelegateDiagnostic(t, out)
	if code != output.ExitCodeFor(output.CodeCapabilityUnavailable) || issue.Details["diagnostic_code"] != "delegate_multica_result_unavailable" {
		t.Fatalf("exit=%d %s", code, out)
	}
	if strings.Contains(out, "private-prompt") || strings.Contains(out, "candidate_selectors") {
		t.Fatalf("selection error masked capability: %s", out)
	}
	assertDelegateNoExecutionSideEffects(t, f)
}

func TestReadDelegateRequestFileErrorsDoNotExposeContents(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	if err := os.WriteFile(f.request, []byte(strings.Repeat("secret", delegation.MaxRequestBytes)), 0600); err != nil {
		t.Fatal(err)
	}
	_, issue := readDelegateRequest(f.request)
	if issue == nil || issue.Details["diagnostic_code"] != "delegate_request_file" || strings.Contains(issue.Error(), "secret") {
		t.Fatalf("issue=%#v", issue)
	}
	if err := os.Remove(f.request); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.config, f.request); err != nil {
		t.Fatal(err)
	}
	_, issue = readDelegateRequest(f.request)
	if issue == nil || issue.Details["diagnostic_code"] != "delegate_request_file" {
		t.Fatalf("symlink issue=%#v", issue)
	}
}

func TestDelegateTopLevelSettingsDiagnosticNamesSelectorLocation(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	if err := os.WriteFile(f.request, []byte(`{"schema_version":1,"request_key":"test-work-1","selector":{"model":"gpt-6.1-sol"},"settings":{"access":"read_only"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, out := f.invoke("private-prompt", "--plan", "--authority", "native")
	if code != output.ExitCodeFor(output.CodeUsage) {
		t.Fatalf("exit=%d %s", code, out)
	}
	issue := decodeDelegateDiagnostic(t, out)
	if issue.Details["field_path"] != "$.settings" || issue.Details["suggested_field_path"] != "$.selector.settings" {
		t.Fatalf("issue=%#v", issue)
	}
	suggestion, _ := issue.Details["suggestion"].(string)
	if !strings.Contains(suggestion, "move it to $.selector.settings") {
		t.Fatalf("suggestion=%q", suggestion)
	}
	example, _ := json.Marshal(issue.Details["example_request"])
	if _, err := delegation.DecodeRequest(example); err != nil {
		t.Fatalf("invalid example: %s %v", example, err)
	}
	assertDelegateNoExecutionSideEffects(t, f)
}

// Agents put labels in the request file and only see the generic unknown-field
// error. The repair must name --label and carry a copyable delegate argv.
func TestDelegateLabelsDiagnosticRepairsWithLabelFlag(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	if err := os.WriteFile(f.request, []byte(`{"schema_version":1,"request_key":"test-work-1","selector":{"family":"grok"},"labels":["label-alpha","label-beta"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, out := f.invoke("private-prompt", "--plan", "--authority", "native")
	if code != output.ExitCodeFor(output.CodeUsage) {
		t.Fatalf("exit=%d %s", code, out)
	}
	issue := decodeDelegateDiagnostic(t, out)
	if issue.Details["field_path"] != "$.labels" || issue.Details["violation"] != "unknown_field" {
		t.Fatalf("issue=%#v", issue)
	}
	suggestion, _ := issue.Details["suggestion"].(string)
	if !strings.Contains(suggestion, "--label <value>") || !strings.Contains(suggestion, "repeatable") {
		t.Fatalf("suggestion=%q", suggestion)
	}
	if _, ok := issue.Details["suggested_field_path"]; ok {
		t.Fatalf("labels are not a request path: %#v", issue.Details)
	}
	if issue.Details["repair_flag"] != "--label" {
		t.Fatalf("repair_flag=%#v", issue.Details["repair_flag"])
	}
	// The fixture selects a nondefault --config and --journal; the repair must
	// rerun in that scope, not against the default profile and journal.
	want := []string{"agentctl", "--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--plan", "--authority", "native", "--label", "<label>"}
	if len(issue.NextActions) != 1 || !reflect.DeepEqual(issue.NextActions[0].Argv, want) {
		t.Fatalf("actions=%#v", issue.NextActions)
	}
	if action := issue.NextActions[0]; action.SideEffectClass != output.ExternalSideEffect || !action.Mutates ||
		!strings.Contains(strings.Join(action.Preconditions, " "), "remove $.labels from the request file") {
		t.Fatalf("action=%#v", action)
	}
	if strings.Contains(out, "label-alpha") || strings.Contains(out, "label-beta") || strings.Contains(out, "private-prompt") {
		t.Fatalf("leaked request content: %s", out)
	}
	assertDelegateNoExecutionSideEffects(t, f)
}

func TestDelegateSelectorAccountDiagnosticNamesPoolAlternative(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	if err := os.WriteFile(f.request, []byte(`{"schema_version":1,"request_key":"test-work-1","selector":{"family":"grok","account":"private-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, out := f.invoke("private-prompt", "--plan", "--authority", "native")
	if code != output.ExitCodeFor(output.CodeUsage) {
		t.Fatalf("exit=%d %s", code, out)
	}
	issue := decodeDelegateDiagnostic(t, out)
	if issue.Details["field_path"] != "$.selector.account" {
		t.Fatalf("issue=%#v", issue)
	}
	suggestion, _ := issue.Details["suggestion"].(string)
	if !strings.Contains(suggestion, "accounts are not selectable") || !strings.Contains(suggestion, `{"pool":"<name>"}`) || !strings.Contains(suggestion, "name a model") {
		t.Fatalf("suggestion=%q", suggestion)
	}
	if issue.Details["suggested_field_path"] != "$.selector.pool" {
		t.Fatalf("suggested=%#v", issue.Details["suggested_field_path"])
	}
	if _, ok := issue.Details["repair_flag"]; ok || hasArgvRepair(issue.NextActions) {
		t.Fatalf("account is not an argv repair: %#v %#v", issue.Details, issue.NextActions)
	}
	example, _ := json.Marshal(issue.Details["example_request"])
	if _, err := delegation.DecodeRequest(example); err != nil {
		t.Fatalf("invalid example: %s %v", example, err)
	}
	if strings.Contains(out, "private-account") {
		t.Fatalf("leaked account: %s", out)
	}
	assertDelegateNoExecutionSideEffects(t, f)
}

func TestDelegateTopLevelFlagFieldDiagnosticNamesFlag(t *testing.T) {
	for _, tc := range []struct{ field, value, flag string }{
		{"timeout", `"10m"`, "--timeout"},
		{"cwd", `"/tmp"`, "--cwd"},
		{"detach", `true`, "--detach"},
		{"wait", `true`, "--wait"},
		{"request_file", `"r.json"`, "--request-file"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			f := newDelegateFixture(t, delegateSuccessScript)
			if err := os.WriteFile(f.request, []byte(`{"schema_version":1,"request_key":"test-work-1","selector":{"family":"grok"},"`+tc.field+`":`+tc.value+`}`), 0600); err != nil {
				t.Fatal(err)
			}
			code, out := f.invoke("private-prompt", "--plan", "--authority", "native")
			if code != output.ExitCodeFor(output.CodeUsage) {
				t.Fatalf("exit=%d %s", code, out)
			}
			issue := decodeDelegateDiagnostic(t, out)
			suggestion, _ := issue.Details["suggestion"].(string)
			if issue.Details["field_path"] != "$."+tc.field || !strings.Contains(suggestion, "delegate flag, not a request field") || !strings.Contains(suggestion, tc.flag) {
				t.Fatalf("issue=%#v", issue)
			}
			// No argv: appending these flags can duplicate a required flag or
			// combine exclusive ones (--wait with --plan), so only the hint ships.
			if issue.Details["repair_flag"] != tc.flag || hasArgvRepair(issue.NextActions) {
				t.Fatalf("actions=%#v", issue.NextActions)
			}
			assertDelegateNoExecutionSideEffects(t, f)
		})
	}
}

func hasArgvRepair(actions []output.NextAction) bool {
	for _, action := range actions {
		for _, precondition := range action.Preconditions {
			if strings.Contains(precondition, "belongs on the delegate command line") {
				return true
			}
		}
	}
	return false
}
