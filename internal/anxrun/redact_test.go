package anxrun

import (
	"strings"
	"testing"
)

// credentialFixtures are deliberately split so no contiguous secret-shaped
// string enters the repository (GitHub push protection scans source bytes);
// at runtime each joins into the full shape the redactor must drop.
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

func TestRedactSummaryDropsCredentialLines(t *testing.T) {
	for _, credential := range credentialFixtures {
		got := RedactSummary("done\n"+credential+"\nnext", DefaultSummaryMaxBytes)
		if got.OmittedReason != "" || strings.Contains(got.Summary, credential) {
			t.Fatalf("credential line survived: %q -> %#v", credential, got)
		}
		if got.Summary != "done\nnext" {
			t.Fatalf("unexpected remaining summary for %q: %q", credential, got.Summary)
		}
	}
}

func TestRedactSummaryDropsEnvAssignments(t *testing.T) {
	got := RedactSummary("ENV_VALUE=secretstuff\nstatus: ok", DefaultSummaryMaxBytes)
	if got.OmittedReason != "" || got.Summary != "status: ok" {
		t.Fatalf("env assignment not dropped: %#v", got)
	}
}

func TestRedactSummaryReplacesPathsHostsQueries(t *testing.T) {
	input := "edited /Users/david/repo/file.go and ~/notes.txt\nsee (/Volumes/Ephemeral/disk)\nfetched https://github.com/o/r/pull/104?token=abc\nhost pve3.tailnet.ts.net and 10.1.2.3 reachable"
	got := RedactSummary(input, DefaultSummaryMaxBytes)
	want := "edited <path> and <path>\nsee (<path>)\nfetched https://github.com/o/r/pull/104\nhost <host> and <host> reachable"
	if got.OmittedReason != "" || got.Summary != want {
		t.Fatalf("summary mismatch:\n got: %q\nwant: %q (omitted=%q)", got.Summary, want, got.OmittedReason)
	}
}

func TestRedactSummaryKeepsPublicURLsAndDelimiters(t *testing.T) {
	got := RedactSummary("PR https://github.com/Git-on-my-level/agentctl/pull/104 merged", DefaultSummaryMaxBytes)
	if got.Summary != "PR https://github.com/Git-on-my-level/agentctl/pull/104 merged" {
		t.Fatalf("public url mangled: %q", got.Summary)
	}
}

func TestRedactSummaryTruncatesWithMarker(t *testing.T) {
	body := strings.Repeat("a", 3000)
	got := RedactSummary(body, 2048)
	if len(got.Summary) != 2048 {
		t.Fatalf("summary length = %d", len(got.Summary))
	}
	if !strings.HasSuffix(got.Summary, truncationMarker) {
		t.Fatalf("missing truncation marker: %q", got.Summary[len(got.Summary)-40:])
	}
	if got.OmittedReason != "" {
		t.Fatalf("unexpected omission: %q", got.OmittedReason)
	}
}

func TestRedactSummaryTruncatesOnRuneBoundary(t *testing.T) {
	body := strings.Repeat("héllo", 600) // 2-byte é
	got := RedactSummary(body, 2048)
	if len(got.Summary) > 2048 {
		t.Fatalf("summary exceeds bound: %d", len(got.Summary))
	}
	for _, r := range got.Summary {
		if r == 0xFFFD {
			t.Fatal("summary split a rune")
		}
	}
}

func TestRedactSummaryOmitsAmbiguousSurvivingCredential(t *testing.T) {
	if SummaryOmittedRedactionFailed != "redaction_failed" {
		t.Fatal("omission reason constant drifted")
	}
	// A partial credential shape (shorter than the full known token, so the
	// whole line is not dropped) is ambiguous: the summary is omitted
	// entirely rather than shipped with a maybe-leak.
	got := RedactSummary("used sk-"+"abcdef01234567 earlier", DefaultSummaryMaxBytes)
	if got.OmittedReason != SummaryOmittedRedactionFailed || got.Summary != "" {
		t.Fatalf("expected omission, got %#v", got)
	}
	// A benign token-ish word without a credential prefix survives.
	got = RedactSummary("task id task-12345678 done", DefaultSummaryMaxBytes)
	if got.OmittedReason != "" || got.Summary != "task id task-12345678 done" {
		t.Fatalf("benign text omitted: %#v", got)
	}
}

func TestRedactSummaryEmpty(t *testing.T) {
	got := RedactSummary("   \n  ", DefaultSummaryMaxBytes)
	if got.Summary != "" || got.OmittedReason != "" {
		t.Fatalf("blank input should project blank: %#v", got)
	}
}

func TestSanitizeLink(t *testing.T) {
	accepted := map[string]Link{
		"https://github.com/o/r/pull/104":                  {Kind: LinkKindURL, Ref: "https://github.com/o/r/pull/104"},
		"pr: https://gitlab.com/o/r/-/merge_requests/7":    {Kind: LinkKindPR, Ref: "https://gitlab.com/o/r/-/merge_requests/7"},
		"commit: 0123456789abcdef0123456789abcdef01234567": {Kind: LinkKindCommit, Ref: "0123456789abcdef0123456789abcdef01234567"},
		"issue: https://github.com/o/r/issues/5":           {Kind: LinkKindIssue, Ref: "https://github.com/o/r/issues/5"},
	}
	for ref, want := range accepted {
		got, ok := SanitizeLink(ref)
		if !ok || got != want {
			t.Fatalf("SanitizeLink(%q) = %#v, %v; want %#v", ref, got, ok, want)
		}
	}
	rejected := []string{
		"",
		"http://github.com/o/r/pull/104",
		"https://evil.example.com/pull/1",
		"https://github.com/o/r/pull/104?token=abc",
		"https://user:pass@gitlab.com/x",
		"pr: 104",
		"pr: http://github.com/o/r/pull/104",
		"commit: not-a-sha",
		"commit: 0123456789abcdef0123456789abcdef012345678",
		strings.Repeat("a", MaxLinkRef+1),
	}
	for _, ref := range rejected {
		if _, ok := SanitizeLink(ref); ok {
			t.Fatalf("SanitizeLink(%q) unexpectedly accepted", ref)
		}
	}
}

func TestExtractLinksBoundedOrderedDeduped(t *testing.T) {
	body := "intro\ncommit: 0123456789abcdef0123456789abcdef01234567\nhttps://github.com/o/r/pull/104\ncommit: 0123456789abcdef0123456789abcdef01234567\nhttps://internal.ts.net/thing\nnot a link\n"
	links := ExtractLinks(body)
	if len(links) != 2 {
		t.Fatalf("links = %#v", links)
	}
	if links[0].Kind != LinkKindCommit || links[1].Ref != "https://github.com/o/r/pull/104" {
		t.Fatalf("order or content wrong: %#v", links)
	}
	var many []string
	for i := 0; i < 30; i++ {
		many = append(many, "https://github.com/o/r/pull/"+strings.Repeat("9", 0+1)+string(rune('0'+i%10)))
	}
	if got := ExtractLinks(strings.Join(many, "\n")); len(got) > MaxLinks {
		t.Fatalf("link bound exceeded: %d", len(got))
	}
}

func TestSanitizeHeadAfter(t *testing.T) {
	if v, err := SanitizeHeadAfter(""); err != nil || v != "" {
		t.Fatalf("empty head_after rejected: %q %v", v, err)
	}
	if v, err := SanitizeHeadAfter("0123456789abcdef0123456789abcdef01234567"); err != nil || v != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("valid head_after rejected: %q %v", v, err)
	}
	if _, err := SanitizeHeadAfter("HEAD"); err == nil {
		t.Fatal("non-sha head_after accepted")
	}
}
