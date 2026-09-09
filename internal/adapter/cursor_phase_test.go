package adapter

import (
	"context"
	"strings"
	"testing"
	"time"
)

func assertCursorPhaseProgress(t *testing.T, line string) {
	t.Helper()
	obs := (cursorParser{}).Parse([]byte(line), false)
	if obs.Terminal {
		t.Fatalf("line %s marked terminal: %#v", line, obs)
	}
	if obs.State != StateRunning {
		t.Fatalf("line %s state=%s want running", line, obs.State)
	}
	if obs.Kind != "progress" {
		t.Fatalf("line %s kind=%s want progress", line, obs.Kind)
	}
	if obs.Content != "" || obs.Summary != "" {
		t.Fatalf("line %s leaked answer content: content=%q summary=%q", line, obs.Content, obs.Summary)
	}
}

func TestCursorThinkingCompletedIsProgressNotTerminal(t *testing.T) {
	line := `{"type":"thinking","subtype":"completed","session_id":"fixture","text":"private reasoning"}`
	assertCursorPhaseProgress(t, line)
	obs := (cursorParser{}).Parse([]byte(line), false)
	if obs.SessionID != "fixture" || obs.Data["type"] != "thinking" || obs.Data["subtype"] != "completed" {
		t.Fatalf("phase lost native metadata: %#v", obs)
	}
	if _, exists := obs.Data["text"]; exists {
		t.Fatal("reasoning text leaked into phase metadata")
	}
}

func TestCursorPhaseSubtypeCompletionsAreNotTaskTerminal(t *testing.T) {
	parser := cursorParser{}
	for _, line := range []string{
		`{"type":"tool_call","subtype":"completed"}`,
		`{"type":"tool_result","subtype":"done"}`,
		`{"type":"assistant","subtype":"completed"}`,
		`{"type":"tool_call","subtype":"failed","error":"tool invocation failed"}`,
	} {
		obs := parser.Parse([]byte(line), false)
		if obs.Terminal {
			t.Fatalf("line %s marked terminal: %#v", line, obs)
		}
		if obs.State == StateCompleted || obs.State == StateFailed {
			t.Fatalf("line %s reached terminal state %s", line, obs.State)
		}
		if obs.Kind != "progress" {
			t.Fatalf("line %s kind=%s want progress", line, obs.Kind)
		}
		if obs.Content != "" || obs.Summary != "" {
			t.Fatalf("line %s leaked answer content: content=%q summary=%q", line, obs.Content, obs.Summary)
		}
	}
}

func TestCursorNestedToolOutcomeIsNotOverallAnswer(t *testing.T) {
	parser := cursorParser{}
	for _, line := range []string{
		`{"type":"tool_result","subtype":"completed","result":"tool secret"}`,
		`{"type":"tool_call","subtype":"failed","error":"shell denied"}`,
	} {
		obs := parser.Parse([]byte(line), false)
		if obs.Terminal {
			t.Fatalf("line %s marked terminal: %#v", line, obs)
		}
		if obs.Content != "" || obs.Summary != "" {
			t.Fatalf("line %s retained tool outcome as answer: content=%q summary=%q", line, obs.Content, obs.Summary)
		}
		if obs.Error != "" && obs.State == StateFailed {
			t.Fatalf("line %s promoted tool error to task failure: %#v", line, obs)
		}
	}
}

func TestCursorTopLevelResultRemainsTerminal(t *testing.T) {
	parser := cursorParser{}
	success := parser.Parse([]byte(`{"type":"result","result":"direct answer","is_error":false}`), false)
	if !success.Terminal || !success.Success || success.State != StateCompleted {
		t.Fatalf("success result=%#v", success)
	}
	if success.Content != "direct answer" {
		t.Fatalf("success content=%q", success.Content)
	}
	if success.ContentSource != "assistant_terminal_result" {
		t.Fatalf("success content source=%q", success.ContentSource)
	}

	failure := parser.Parse([]byte(`{"type":"result","is_error":true,"error":"approval denied"}`), false)
	if !failure.Terminal || failure.Success || failure.State != StateFailed {
		t.Fatalf("failure result=%#v", failure)
	}
	if failure.Error != "approval denied" {
		t.Fatalf("failure error=%q", failure.Error)
	}
}

func TestCursorAssistantContentRemainsAvailableWithoutFinishingTask(t *testing.T) {
	obs := (cursorParser{}).Parse([]byte(`{"type":"assistant","subtype":"completed","message":{"role":"assistant","content":[{"type":"text","text":"assistant answer"}]}}`), false)
	if obs.Terminal {
		t.Fatalf("assistant message marked terminal: %#v", obs)
	}
	if obs.Content != "assistant answer" || obs.ContentSource != "assistant" {
		t.Fatalf("assistant content unavailable: %#v", obs)
	}
}

func TestCursorThinkingCompletedThenNonzeroExitWithoutResultFails(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"thinking","subtype":"completed"}'; exit 17`)
	got, err := NewCursor().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil {
		t.Fatal("missing result")
	}
	if got.Result.State == StateCompleted || got.Result.Success {
		t.Fatalf("thinking completed must not complete task: %#v", got.Result)
	}
	if got.Result.State != StateFailed {
		t.Fatalf("nonzero exit without result should fail, got %#v", got.Result)
	}
}

func TestCursorThinkingCompletedThenResultReturnsActualAnswer(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"thinking","subtype":"completed"}' '{"type":"result","result":"actual answer","is_error":false}'`)
	got, err := NewCursor().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.State != StateCompleted {
		t.Fatalf("unexpected result: %#v", got.Result)
	}
	if got.Result.Content != "actual answer" {
		t.Fatalf("content=%q", got.Result.Content)
	}
}

func TestCursorToolPhaseCompletionDoesNotBecomeFinalAnswer(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"tool_result","subtype":"completed","result":"tool secret"}' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"final answer"}]}}' '{"type":"result","subtype":"success","is_error":false}'`)
	got, err := NewCursor().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.Content != "final answer" {
		t.Fatalf("unexpected Cursor result: %#v", got.Result)
	}
	if strings.Contains(got.Result.Content, "tool secret") {
		t.Fatalf("tool phase outcome leaked into final answer: %#v", got.Result)
	}
}

func TestCursorToolCompletionCannotSupplyEmptyTaskResult(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"tool_result","subtype":"completed","result":"tool secret"}' '{"type":"result","subtype":"success","is_error":false}'`)
	got, err := NewCursor().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.Content != "" {
		t.Fatalf("tool output substituted for missing assistant answer: %#v", got.Result)
	}
}

func TestCursorThinkingCompletionThenCleanExitIsOrphaned(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"thinking","subtype":"completed"}'`)
	got, err := NewCursor().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Success || got.Result.State != StateOrphaned {
		t.Fatalf("clean exit without task result became success: %#v", got.Result)
	}
}
