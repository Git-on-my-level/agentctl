package model

import (
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

func TestTaskContractValidate(t *testing.T) {
	acceptance, err := ids.NewContextID(ids.CryptoGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	valid := TaskContract{
		ObjectiveSummary:      "Diagnose the failing service",
		SideEffectBoundary:    "read_only",
		AcceptanceRef:         &acceptance,
		ExpectedArtifactKinds: []string{"root_cause_report"},
		Continuation:          &Continuation{SameSessionRequired: true},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid contract: %v", err)
	}
	for name, contract := range map[string]TaskContract{
		"empty":              {},
		"oversized_summary":  {ObjectiveSummary: strings.Repeat("x", 2049)},
		"invalid_boundary":   {SideEffectBoundary: "Read Only"},
		"invalid_artifact":   {ExpectedArtifactKinds: []string{"Report"}},
		"duplicate_artifact": {ExpectedArtifactKinds: []string{"report", "report"}},
		"invalid_digest":     {Provenance: &ExecutionProvenance{ContextDigest: "sha256:nope"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := contract.Validate(); err == nil {
				t.Fatal("invalid task contract was accepted")
			}
		})
	}
}

func TestTaskContractIsImmutableAcrossExecutionRevisions(t *testing.T) {
	previous := fixtureExecution(t)
	previous.TaskContract = &TaskContract{ObjectiveSummary: "bounded objective", ExpectedArtifactKinds: []string{"report"}}
	next := previous
	next.Revision++
	next.TaskContract = &TaskContract{ObjectiveSummary: "different objective", ExpectedArtifactKinds: []string{"report"}}
	if err := ValidateTransition(previous, next); err == nil || !strings.Contains(err.Error(), "task contract is immutable") {
		t.Fatalf("transition error=%v", err)
	}
}
