package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExecutionCallerValidatesAndIsImmutable(t *testing.T) {
	for _, harness := range []CallerHarness{CallerHermes, CallerClaudeCode, CallerCodex, CallerCursor, CallerOMP, CallerZCode, CallerDevin, CallerOther} {
		value := fixtureExecution(t)
		value.Caller = &ExecutionCaller{Harness: harness, Provenance: CallerDeclared}
		if err := value.Validate(); err != nil {
			t.Fatalf("%s: %v", harness, err)
		}
	}
	for _, caller := range []ExecutionCaller{{Harness: "unknown", Provenance: CallerDeclared}, {Harness: CallerCodex, Provenance: "inferred"}, {Harness: "Codex", Provenance: CallerDeclared}, {Harness: CallerCodex}} {
		value := fixtureExecution(t)
		value.Caller = &caller
		if err := value.Validate(); err == nil {
			t.Fatalf("invalid caller accepted: %#v", caller)
		}
	}
	previous := fixtureExecution(t)
	previous.Caller = &ExecutionCaller{Harness: CallerHermes, Provenance: CallerDeclared}
	next := previous
	next.Revision++
	next.Caller = &ExecutionCaller{Harness: CallerCodex, Provenance: CallerDeclared}
	if err := ValidateTransition(previous, next); err == nil || !strings.Contains(err.Error(), "caller is immutable") {
		t.Fatalf("caller mutation err=%v", err)
	}
	next.Caller = nil
	if err := ValidateTransition(previous, next); err == nil {
		t.Fatal("caller removal accepted")
	}
	previous.Caller = nil
	next.Caller = &ExecutionCaller{Harness: CallerCodex, Provenance: CallerDeclared}
	if err := ValidateTransition(previous, next); err == nil {
		t.Fatal("historical unknown caller backfill accepted")
	}
}

func TestExecutionWithoutCallerRemainsUnknown(t *testing.T) {
	value := fixtureExecution(t)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"caller"`) {
		t.Fatalf("undeclared caller exported: %s", data)
	}
	var restored Execution
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Caller != nil {
		t.Fatalf("historical caller inferred: %#v", restored.Caller)
	}
	if err := restored.Validate(); err != nil {
		t.Fatal(err)
	}
}
