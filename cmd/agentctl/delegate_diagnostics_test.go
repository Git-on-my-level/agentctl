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
			if !reflect.DeepEqual(issue.Details["allowed_keys"], []any{"access", "effort", "speed"}) {
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
