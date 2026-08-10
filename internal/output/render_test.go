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
