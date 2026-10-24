package anxrun

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
)

func TestReview3EmbeddedCredentialsAndEntropy(t *testing.T) {
	for _, value := range []string{strings.Repeat("a", 38) + "glpat-" + "abcdefghijklmnopqrst", strings.Repeat("a", 50) + "ghs_" + strings.Repeat("b", 36), strings.Repeat("a", 60) + "bC3dE5fG7hJ9kL1mN2pQ4rS6"} {
		if stringCode(value, 128, false) == 0 {
			t.Fatal("embedded credential/entropy escaped")
		}
	}
	if refs := ResolveCards([]string{CardLabelPrefix + strings.Repeat("a", 38) + "glpat-" + "abcdefghijklmnopqrst"}); len(refs) != 0 {
		t.Fatal("embedded credential slug escaped")
	}
}

func TestReview3GeneratedFieldGrammars(t *testing.T) {
	r := testRegistration("exec-a", PhaseStart, 1)
	r.BuiltAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Allowlisted(r, nil); err == nil {
		t.Fatal("invalid generated timestamp accepted")
	}
	r.BuiltAt = time.Now().UTC()
	r.Sequence = -1
	if _, err := Allowlisted(r, nil); err == nil {
		t.Fatal("invalid sequence accepted")
	}
	r.Sequence = 1
	local := time.Date(2026, 10, 24, 0, 0, 0, 0, time.FixedZone("private-zone", 3600))
	r.StartedAt = &local
	safe, err := Allowlisted(r, nil)
	if err != nil || safe.StartedAt.Location() != time.UTC {
		t.Fatal("noncanonical timestamp")
	}
}

func TestReview3NoResultTextEvidence(t *testing.T) {
	for _, repo := range []string{"100.101.102.103", "pve3.tailnet.ts.net", "agentctl"} {
		text := `{"kind":"pr","owner":"Git-on-my-level","repo":"` + repo + `","number":104}`
		o := ProjectOutcome(model.Outcome{Availability: model.OutcomeStored, Content: &model.OutcomeContent{Text: text}})
		if len(o.Links) != 0 {
			t.Fatal("result text was treated as authoritative evidence")
		}
	}
}

func TestReview3CoordinatesNeverSpooledWithoutAuthority(t *testing.T) {
	for _, repo := range []string{"100.101.102.103", "pve3.tailnet.ts.net"} {
		r := testRegistration("exec-a", PhaseTerminal, 1)
		r.Outcome = &Outcome{Availability: AvailabilityStored, Links: []Link{{Kind: LinkKindPR, Owner: "Git-on-my-level", Repo: repo, Number: 104}}}
		entry, err := testSpool(t).Enqueue(r)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(entry)
		if strings.Contains(string(raw), repo) {
			t.Fatal("unverified network coordinate persisted")
		}
	}
	r := testRegistration("exec-a", PhaseStart, 1)
	entry, err := testSpool(t).Enqueue(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(entry.Registration.CardRefs) != 0 {
		t.Fatal("unconfirmed card ref persisted")
	}
}
