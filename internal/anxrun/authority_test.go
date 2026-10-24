package anxrun

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReview3CardAuthorityCacheAndOfflineDrop(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{}
	options := CLIOptions{WorkspaceAlias: "target", CacheDir: root}
	sender := &CLISender{options: options, run: runner.run}
	r := testRegistration("exec-a", PhaseStart, 1)
	if err := sender.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	listCalls := 0
	for _, args := range runner.argvs {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "api cards.list") {
			listCalls++
			if strings.Contains(joined, "card:") || !strings.Contains(joined, "--workspace target") {
				t.Fatal("unverified slug left host during confirmation")
			}
		}
	}
	if listCalls != 1 {
		t.Fatalf("catalog was not cached within delivery batch: %d", listCalls)
	}
	path := filepath.Join(root, "confirmed-cards.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatal("cache permissions")
	}
	if strings.Contains(string(raw), "summary") || strings.Contains(string(raw), "private-card-text") {
		t.Fatal("card prose entered cache")
	}
	// An old disk snapshot never grants offline authority to a new batch.
	offline := &fakeRunner{envelopes: []string{`{"ok":false,"error":{"code":"sessions_unavailable"}}`}}
	sender = &CLISender{options: options, run: offline.run}
	if err := sender.Send(context.Background(), r); err == nil {
		t.Fatal("stale cache authorized offline delivery")
	}
	for _, args := range offline.argvs {
		if strings.Contains(strings.Join(args, " "), "watch-redesign") {
			t.Fatal("unverified card left host")
		}
	}
}

func TestReview3UnknownOrSecretCardNeverLeavesHost(t *testing.T) {
	secret := strings.Repeat("a", 38) + "glpat-" + "abcdefghijklmnopqrst"
	for _, slug := range []string{"unknown-card", secret} {
		runner := &fakeRunner{}
		s := &CLISender{run: runner.run, options: CLIOptions{CacheDir: t.TempDir()}}
		r := testRegistration("exec-a", PhaseStart, 1)
		r.CardRefs = []string{"card:" + slug}
		if err := s.Send(context.Background(), r); err == nil {
			t.Fatal("unverified card delivered")
		}
		for i, args := range runner.argvs {
			if strings.Contains(strings.Join(args, " ")+runner.stdins[i], slug) {
				t.Fatal("candidate used in outbound lookup or write")
			}
		}
	}
}

func TestReview3RepositoryAuthority(t *testing.T) {
	r := testRegistration("exec-a", PhaseTerminal, 1)
	r.Outcome = &Outcome{Availability: AvailabilityStored, Links: []Link{{Kind: LinkKindPR, Owner: "Git-on-my-level", Repo: "agentctl", Number: 104}}}
	confirmed := map[string]ConfirmedCard{"card:watch-redesign": {Ref: "card:watch-redesign", ThreadID: "thread-test"}}
	for name, policy := range map[string]Verification{
		"none":           {Cards: confirmed},
		"org":            {Cards: confirmed, AllowedOrgs: []string{"Git-on-my-level"}},
		"origin":         {Cards: confirmed, Origin: &Repository{Owner: "Git-on-my-level", Name: "agentctl"}},
		"allowed-origin": {Cards: confirmed, AllowedOrgs: []string{"Git-on-my-level"}, Origin: &Repository{Owner: "Git-on-my-level", Name: "agentctl"}},
		"other-origin":   {Cards: confirmed, Origin: &Repository{Owner: "Git-on-my-level", Name: "other"}},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			s := &CLISender{options: CLIOptions{Verification: &policy}, run: runner.run}
			if err := s.Send(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			var body struct {
				Event struct {
					Payload struct {
						Text string `json:"text"`
					} `json:"payload"`
				} `json:"event"`
			}
			_ = json.Unmarshal([]byte(runner.stdins[len(runner.stdins)-1]), &body)
			var got Registration
			_ = json.Unmarshal([]byte(body.Event.Payload.Text), &got)
			want := name == "allowed-origin"
			if (len(got.Outcome.Links) == 1) != want {
				t.Fatalf("repository authority not enforced: %+v", got.Outcome)
			}
		})
	}
	for _, repo := range []string{"100.101.102.103", "pve3.tailnet.ts.net", strings.Repeat("a", 40) + "ghs_" + strings.Repeat("b", 36)} {
		r.Outcome.Links[0].Repo = repo
		runner := &fakeRunner{}
		s := &CLISender{options: CLIOptions{AllowedOrgs: []string{"Git-on-my-level"}}, run: runner.run}
		if err := s.Send(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		for _, raw := range runner.stdins {
			if strings.Contains(raw, repo) {
				t.Fatal("network/credential coordinate escaped despite org policy")
			}
		}
	}
}

func TestReview3OriginIsLocalConfigurationOnly(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for _, remote := range []string{"https://github.com/Git-on-my-level/agentctl.git", "git@github.com:Git-on-my-level/agentctl.git", "ssh://git@github.com/Git-on-my-level/agentctl.git"} {
		if out, err := exec.Command("git", "-C", root, "config", "--local", "remote.origin.url", remote).CombinedOutput(); err != nil {
			t.Fatalf("config: %v %s", err, out)
		}
		origin := OriginRepository(context.Background(), root)
		if origin == nil || origin.Owner != "Git-on-my-level" || origin.Name != "agentctl" {
			t.Fatalf("origin parse: %+v", origin)
		}
	}
	for _, remote := range []string{"https://user:private@github.com/o/r.git", "https://github.com/o/100.101.102.103.git", "https://github.com/o/pve3.tailnet.ts.net", "https://github.com/o/r.git?token=x", "git@internal.ts.net:o/r.git", "file:///private/path"} {
		if origin := ParseOrigin(remote); origin != nil {
			t.Fatal("unsafe origin accepted")
		}
	}
	if OriginRepository(context.Background(), filepath.Join(root, "missing")) != nil {
		t.Fatal("missing origin guessed")
	}
}
