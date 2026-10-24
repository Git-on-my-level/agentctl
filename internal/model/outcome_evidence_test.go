package model

import (
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

func TestOutcomeTypedEvidenceContract(t *testing.T) {
	id, _ := ids.NewExecutionID(ids.CryptoGenerator{})
	base := Outcome{SchemaVersion: 1, ExecutionID: id, Revision: 1, State: StateCompleted, RecordedAt: time.Now().UTC(), Source: "fixture", ResultRef: "fixture", Availability: OutcomeStored, Content: &OutcomeContent{MediaType: "text/plain", Text: "done", Preview: "done", Bytes: 4}}
	for _, e := range []OutcomeEvidence{{Kind: "pr", Owner: "Git-on-my-level", Repo: "agentctl", Number: 104}, {Kind: "issue", Owner: "o", Repo: "r", Number: 1}, {Kind: "commit", SHA: strings.Repeat("a", 40)}} {
		o := base
		o.Evidence = []OutcomeEvidence{e}
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []OutcomeEvidence{{Kind: "url", Repo: "private"}, {Kind: "pr", Owner: "o", Repo: "r", Number: 0}, {Kind: "issue", Owner: "o", Repo: "r", Number: 2147483648}, {Kind: "pr", Owner: "o", Repo: strings.Repeat("r", 101), Number: 1}, {Kind: "commit", SHA: "HEAD"}, {Kind: "commit", SHA: strings.Repeat("a", 40), Owner: "o"}} {
		o := base
		o.Evidence = []OutcomeEvidence{e}
		if err := o.Validate(); err == nil {
			t.Fatal("invalid typed evidence accepted")
		}
	}
	o := base
	o.Evidence = make([]OutcomeEvidence, 9)
	if o.Validate() == nil {
		t.Fatal("unbounded typed evidence")
	}
}
