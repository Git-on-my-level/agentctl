package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixtureDelegation() *DelegationBinding {
	return &DelegationBinding{RequestSHA256: "sha256:" + strings.Repeat("a", 64), ConfigurationSHA256: "sha256:" + strings.Repeat("b", 64), Requested: json.RawMessage(`{"family":"grok"}`), Resolved: DelegationTarget{Harness: "cursor", Family: "grok", Version: "4.6", Model: "cursor-grok-4.6-high", Host: "workstation", Authority: AuthorityNative, Settings: DelegationSettings{Speed: "regular"}}, Defaulted: []string{"harness", "version", "model", "speed", "host"}}
}
func TestDelegationBindingImmutable(t *testing.T) {
	previous := fixtureExecution(t)
	previous.Delegation = fixtureDelegation()
	if err := previous.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Execution){func(e *Execution) { e.Delegation = nil }, func(e *Execution) { copy := *e.Delegation; copy.Resolved.Model = "other"; e.Delegation = &copy }, func(e *Execution) {
		copy := *e.Delegation
		copy.Requested = json.RawMessage(`{"family":"composer"}`)
		e.Delegation = &copy
	}} {
		next := previous
		next.Revision++
		mutate(&next)
		if err := ValidateTransition(previous, next); err == nil || !strings.Contains(err.Error(), "delegation") {
			t.Fatalf("transition=%v", err)
		}
	}
}
func TestDelegationBindingRejectsInvalidMetadata(t *testing.T) {
	for _, mutate := range []func(*DelegationBinding){func(b *DelegationBinding) { b.RequestSHA256 = "invalid" }, func(b *DelegationBinding) { b.Requested = json.RawMessage(`{"prompt":"secret"}`) }, func(b *DelegationBinding) { b.Resolved.Model = "bad\nvalue" }, func(b *DelegationBinding) { b.Resolved.Authority = "unknown" }, func(b *DelegationBinding) { b.Defaulted = []string{"prompt"} }} {
		binding := fixtureDelegation()
		mutate(binding)
		if err := binding.Validate(); err == nil {
			t.Fatalf("invalid binding accepted: %#v", binding)
		}
	}
}
