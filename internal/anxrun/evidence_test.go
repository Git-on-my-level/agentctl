package anxrun

import (
	"encoding/json"
	"fmt"
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
func TestSanitizeLink(t *testing.T) {
	accepted := map[string]Link{
		`{"kind":"pr","owner":"Git-on-my-level","repo":"agentctl","number":104}`: {Kind: LinkKindPR, Owner: "Git-on-my-level", Repo: "agentctl", Number: 104},
		`{"kind":"issue","owner":"o","repo":"r._-","number":5}`:                  {Kind: LinkKindIssue, Owner: "o", Repo: "r._-", Number: 5},
		"commit: 0123456789abcdef0123456789ABCDEF01234567":                       {Kind: LinkKindCommit, SHA: "0123456789abcdef0123456789abcdef01234567"},
	}
	for ref, want := range accepted {
		if got, ok := SanitizeLink(ref); !ok || got != want {
			t.Fatalf("%q: %+v %v", ref, got, ok)
		}
	}
	for _, ref := range []string{"", "https://github.com/o/r/pull/104", "pr: https://github.com/o/r/pull/7", "issue: https://github.com/o/r/issues/5", "http://github.com/o/r/pull/1", "https://evil.example.com/pull/1", "https://github.com/o/r/pull/1?token=abc", "https://user:pass@gitlab.com/x", "pr: 104", "commit: not-a-sha", "commit: 0123456789abcdef0123456789abcdef012345678", strings.Repeat("a", MaxLinkRef+1), `{"kind":"pr","owner":"o","repo":"r","number":1,"url":"private"}`, `{"kind":"pr","owner":"o","repo":"r","number":1} {}`} {
		if _, ok := SanitizeLink(ref); ok {
			t.Fatalf("accepted %q", ref)
		}
	}
	for _, owner := range []string{"", "-o", "o-", "o--x", "o_x", "o/x", strings.Repeat("o", 40)} {
		if _, code := validateLink(Link{Kind: LinkKindPR, Owner: owner, Repo: "r", Number: 1}); code == 0 {
			t.Fatalf("owner accepted: %s", owner)
		}
	}
	for _, repo := range []string{"", ".", "..", "r.git", "r.wiki", "r/x", "r?token", "r#fragment", "r%20", strings.Repeat("r", 101)} {
		if _, code := validateLink(Link{Kind: LinkKindIssue, Owner: "o", Repo: repo, Number: 1}); code == 0 {
			t.Fatalf("repo accepted: %s", repo)
		}
	}
}
func TestExtractLinksBoundedOrderedDeduped(t *testing.T) {
	body := "intro\ncommit: 0123456789abcdef0123456789abcdef01234567\n" + `{"kind":"pr","owner":"o","repo":"r","number":104}` + "\ncommit: 0123456789abcdef0123456789abcdef01234567\nhttps://internal.ts.net/thing\nnot a link"
	links := ExtractLinks(body)
	if len(links) != 2 || links[0].Kind != LinkKindCommit || links[1].Number != 104 {
		t.Fatalf("%+v", links)
	}
	var many []string
	for i := 1; i <= 30; i++ {
		many = append(many, fmt.Sprintf(`{"kind":"pr","owner":"o","repo":"r","number":%d}`, i))
	}
	if got := ExtractLinks(strings.Join(many, "\n")); len(got) != MaxLinks {
		t.Fatalf("bound: %d", len(got))
	}
	if got := ExtractLinks(strings.Repeat(" ", maxLinkScan) + body); len(got) != 0 {
		t.Fatal("scan exceeded bound")
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
