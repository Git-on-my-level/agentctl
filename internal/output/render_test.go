package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestExitCodeContract(t *testing.T) {
	cases := map[Code]int{CodeUsage: 2, CodeNotFound: 3, CodeAmbiguousReference: 4, CodeCapabilityUnavailable: 5, CodeDependencyUnavailable: 6, CodeAuthenticationRequired: 7, CodeConflict: 8, CodeTimeout: 9, CodeAttentionRequired: 10, CodeExecutionFailed: 11, CodeExecutionCancelled: 12, CodeUnknownState: 13, CodeInternal: 70}
	for code, want := range cases {
		if got := ExitCodeFor(code); got != want {
			t.Fatalf("%s exit=%d want=%d", code, got, want)
		}
	}
}

func TestJSONSuccessIsOneNormativeDocument(t *testing.T) {
	var buffer bytes.Buffer
	renderer := Renderer{Mode: JSON, Writer: &buffer}
	if err := renderer.Success(Success{Result: map[string]any{"state": "running"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buffer.String(), "\n") != 1 {
		t.Fatalf("framing=%q", buffer.String())
	}
	var doc SuccessDocument
	if err := json.Unmarshal(buffer.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.OK || doc.SchemaVersion != 1 || doc.Warnings == nil || doc.NextActions == nil {
		t.Fatalf("document=%#v", doc)
	}
}

func TestRendererAddsGlobalWarningsToSuccessAndFailure(t *testing.T) {
	warning := Warning{Code: "agentctl_update_available", Message: "update available", Details: map[string]any{"latest_version": "v0.3.3", "current_version": "v0.3.2"}}
	var buffer bytes.Buffer
	renderer := (Renderer{Mode: JSON, Writer: &buffer}).WithWarnings(warning)
	if err := renderer.Success(Success{Result: map[string]any{"state": "ready"}}); err != nil {
		t.Fatal(err)
	}
	var success SuccessDocument
	if err := json.Unmarshal(buffer.Bytes(), &success); err != nil || len(success.Warnings) != 1 || success.Warnings[0].Details["latest_version"] != "v0.3.3" {
		t.Fatalf("success=%#v err=%v", success, err)
	}

	buffer.Reset()
	renderer = (Renderer{Mode: JSON, Writer: &buffer}).WithWarnings(warning)
	if err := renderer.Failure(NewError(CodeUsage, "bad input", false)); err != nil {
		t.Fatal(err)
	}
	var failure ErrorDocument
	if err := json.Unmarshal(buffer.Bytes(), &failure); err != nil || len(failure.Warnings) != 1 {
		t.Fatalf("failure=%#v err=%v", failure, err)
	}
}

func TestGlobalWarningIsEmittedOnceForStreamingRenderer(t *testing.T) {
	var buffer bytes.Buffer
	renderer := (Renderer{Mode: JSON, Writer: &buffer}).WithWarnings(Warning{Code: "agentctl_update_available", Message: "update available"})
	if err := renderer.Success(Success{Result: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Success(Success{Result: "second"}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buffer.String(), "agentctl_update_available") != 1 {
		t.Fatalf("warning was not emitted exactly once: %s", buffer.String())
	}
}

func TestTextEscapesAmbiguousValuesAndSortsErrorDetails(t *testing.T) {
	line := RenderLine(Line{Lead: "exec-x", Fields: []Field{{Name: "plain", Value: "running"}, {Name: "quoted", Value: "two words"}, {Name: "syntax", Value: "agentctl <command>"}}})
	if line != `exec-x plain=running quoted="two words" syntax="agentctl <command>"` {
		t.Fatalf("line=%q", line)
	}
	var buffer bytes.Buffer
	err := NewError(CodeConflict, "conflict", false).WithDetail("z", 2).WithDetail("a", "two words")
	if writeErr := (Renderer{Mode: Text, Writer: &buffer}).Failure(err); writeErr != nil {
		t.Fatal(writeErr)
	}
	if got := buffer.String(); got != "error code=conflict exit=8 retryable=false message=conflict a=\"two words\" z=2\n" {
		t.Fatalf("output=%q", got)
	}
}

func TestTextRendersWarningsAfterPrimaryOutput(t *testing.T) {
	var buffer bytes.Buffer
	renderer := Renderer{Mode: Text, Writer: &buffer, Warnings: []Warning{{Code: "agentctl_update_available", Message: "update available", Details: map[string]any{"latest_version": "v0.3.3", "current_version": "v0.3.2"}}}}
	if err := renderer.Success(Success{Lines: []Line{{Lead: "agentctl", Fields: []Field{{Name: "version", Value: "v0.3.2"}}}}}); err != nil {
		t.Fatal(err)
	}
	want := "agentctl version=v0.3.2\nwarning code=agentctl_update_available message=\"update available\" current_version=v0.3.2 latest_version=v0.3.3\n"
	if buffer.String() != want {
		t.Fatalf("output=%q want=%q", buffer.String(), want)
	}
}

func TestNextActionStdinFromStaysOutOfArgv(t *testing.T) {
	action := NextAction{Label: "Pipe prompt", Argv: []string{"agentctl", "run", "--prompt-stdin", "--", "agent"}, StdinFrom: "/tmp/task.md", SideEffectClass: ReadOnly, Preconditions: []string{}}
	var buffer bytes.Buffer
	if err := (Renderer{Mode: JSON, Writer: &buffer}).Failure(NewError(CodeNotFound, "missing", false).WithActions(action)); err != nil {
		t.Fatal(err)
	}
	var document ErrorDocument
	if err := json.Unmarshal(buffer.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	got := document.Error.NextActions[0]
	if got.StdinFrom != "/tmp/task.md" {
		t.Fatalf("stdin_from=%q", got.StdinFrom)
	}
	for _, arg := range got.Argv {
		if arg == "<" || arg == "/tmp/task.md" {
			t.Fatalf("stdin source leaked into argv: %#v", got.Argv)
		}
	}
	buffer.Reset()
	if err := (Renderer{Mode: Text, Writer: &buffer}).Failure(NewError(CodeNotFound, "missing", false).WithActions(action)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), "stdin_from=/tmp/task.md") {
		t.Fatalf("text action lost stdin_from: %q", buffer.String())
	}
}
