package anxrun

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestV4SpoolEvidenceSurvivesDeliveryAsV5(t *testing.T) {
	s := testSpool(t)
	entry, err := s.Enqueue(testRegistration("exec-v4", PhaseTerminal, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Write the previously shipped shape directly, without first passing its
	// evidence through the new normalizer. v4 outboxes retain commit evidence;
	// repository evidence is recovered from the journal at delivery.
	entry.Registration.SchemaVersion = 4
	entry.Registration.NativeSession = nil
	entry.Registration.Outcome = &Outcome{
		Availability: AvailabilityStored,
		Links:        []Link{{Kind: LinkKindCommit, SHA: strings.Repeat("ab", 20)}},
		HeadAfter:    strings.Repeat("cd", 20),
	}
	want := *entry.Registration.Outcome
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.pendingPath(entryKey(entry.ExecutionID, entry.Phase)), raw, 0600); err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("v4 entry unreadable: %v %v", pending, err)
	}
	if pending[0].Registration.SchemaVersion != 5 || !reflect.DeepEqual(*pending[0].Registration.Outcome, want) {
		t.Fatalf("v4 evidence changed on read: %+v", pending[0].Registration)
	}
	runner := &fakeRunner{}
	sender := &CLISender{run: runner.run, Resolve: func(_ context.Context, r Registration) (Registration, *Repository, error) {
		// Recover only the target from the journal; evidence must survive from
		// the old spool, rather than being replaced by a fresh outcome fixture.
		r.CardRefs = []string{"card:watch-redesign"}
		return r, nil, nil
	}}
	result, err := s.Flush(context.Background(), sender, 0)
	if err != nil || result.Delivered != 1 {
		t.Fatalf("v4 delivery failed: %+v %v", result, err)
	}
	got := publicRegistration(t, runner)
	if got.SchemaVersion != 5 || got.Outcome == nil || !reflect.DeepEqual(*got.Outcome, want) {
		t.Fatalf("v4 evidence lost at egress: %+v", got)
	}
	pending, err = s.ListPending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("delivered v4 entry remains pending: %v %v", pending, err)
	}
}

func TestTypedEvidenceSchemaCompatibility(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4, 5, 6} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			r := testRegistration("exec-a", PhaseTerminal, 1)
			r.SchemaVersion = version
			r.Outcome = &Outcome{
				Availability: AvailabilityStored,
				Links: []Link{
					{Kind: LinkKindPR, Owner: "o", Repo: "r", Number: 1},
					{Kind: LinkKindIssue, Owner: "o", Repo: "r", Number: 2},
					{Kind: LinkKindCommit, SHA: strings.Repeat("ab", 20)},
				},
				HeadAfter: strings.Repeat("cd", 20),
			}
			policy := Verification{Origin: &Repository{Host: "forge.local", Owner: "o", Name: "r"}}
			got, err := Allowlisted(r, nil, policy)
			if err != nil {
				t.Fatal(err)
			}
			if got.SchemaVersion != 5 {
				t.Fatalf("not normalized to v5: %+v", got)
			}
			if version == 4 || version == 5 {
				if !reflect.DeepEqual(got.Outcome, r.Outcome) {
					t.Fatalf("compatible evidence changed: %+v", got)
				}
			} else if len(got.Outcome.Links) != 0 || got.Outcome.HeadAfter != "" || got.EgressErrorCode != EgressUnverified {
				t.Fatalf("unsupported schema authorized evidence: %+v", got)
			}
		})
	}
}
