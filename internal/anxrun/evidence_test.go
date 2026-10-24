package anxrun

import (
	"encoding/json"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"strings"
	"testing"
)

var credentialFixtures = []string{
	"AWS_KEY=AKIA" + "IOSFODNN7EXAMPLE",
	"the token is sk-" + "abcdef0123456789abcdef0123456789",
	"github_pat_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
	"slack xox" + "b-1234567890-abcdefghijklmnop",
	"-----BEGIN RSA PRIVATE KEY-----",
	"Authorization: Bearer eyJ" + "hbGciOiJIUzI1NiJ9.payload",
	"gcloud ya29." + "a0ARrdaM-48-bit-token-example-value",
	"maps AIza" + "SyA1234567890abcdefghijklmnopqrstuv",
	`API_TOKEN="a9f3c2d4e5b6a7f8c9d0e1f2a3b4c5d6"`,
	"db_password: h4x0rz1234567890abcdef",
	"SERVICE_KEY=qwertyuiopasdfghjklzxcvbnm12",
}

func TestNoSummaryProjection(t *testing.T) {
	texts := append(append([]string{}, credentialFixtures...), "ENV_VALUE=secretstuff", "edited /Users/david/file and ~/notes.txt", "https://github.com/o/r/pull/1?token=abc", "pve3.tailnet.ts.net 10.1.2.3", "PR https://github.com/o/r/pull/1 merged", strings.Repeat("word ", 600), strings.Repeat("héllo", 600), "used sk-"+"abcdef01234567", "task id task-12345678 done", "   ")
	for _, text := range texts {
		for _, input := range []string{text, "---anx-summary---\n" + text + "\n---end-anx-summary---"} {
			out := ProjectOutcome(model.Outcome{Availability: model.OutcomeStored, Content: &model.OutcomeContent{Text: input}})
			raw, _ := json.Marshal(out)
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			if len(fields) != 1 || fields["availability"] != "stored" {
				t.Fatalf("free text projected: %s", raw)
			}
		}
	}
}
func TestStructuredEvidenceGrammar(t *testing.T) {
	for _, link := range []Link{{Kind: LinkKindPR, Owner: "Git-on-my-level", Repo: "agentctl", Number: 104}, {Kind: LinkKindIssue, Owner: "o", Repo: "r._-", Number: 5}, {Kind: LinkKindCommit, SHA: "0123456789abcdef0123456789abcdef01234567"}} {
		if _, code := validateLink(link); code != 0 {
			t.Fatalf("valid evidence rejected: %+v %d", link, code)
		}
	}
	for _, owner := range []string{"", "-o", "o-", "o--x", "o_x", "o/x", strings.Repeat("o", 40)} {
		if _, code := validateLink(Link{Kind: LinkKindPR, Owner: owner, Repo: "r", Number: 1}); code == 0 {
			t.Fatal("invalid owner accepted")
		}
	}
	for _, repo := range []string{"", ".", "..", "r.git", "r.wiki", "r/x", "r?token", "r#fragment", "r%20", strings.Repeat("r", 101), "100.101.102.103", "pve3.tailnet.ts.net"} {
		if _, code := validateLink(Link{Kind: LinkKindPR, Owner: "o", Repo: repo, Number: 1}); code == 0 {
			t.Fatal("invalid repository accepted")
		}
	}
}
func TestTypedEvidenceIsBounded(t *testing.T) {
	outcome := model.Outcome{Availability: model.OutcomeStored}
	for i := 1; i < 30; i++ {
		outcome.Evidence = append(outcome.Evidence, model.OutcomeEvidence{Kind: "pr", Owner: "o", Repo: "r", Number: i})
	}
	if got := ProjectOutcome(outcome); len(got.Links) != MaxLinks {
		t.Fatal("typed evidence exceeded bound")
	}
}
func TestSanitizeHeadAfter(t *testing.T) {
	for _, v := range []string{"", "0123456789abcdef0123456789abcdef01234567"} {
		if got, err := SanitizeHeadAfter(v); err != nil || got != v {
			t.Fatal(got, err)
		}
	}
	if _, err := SanitizeHeadAfter("HEAD"); err == nil {
		t.Fatal("accepted HEAD")
	}
}
