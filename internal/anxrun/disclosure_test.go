package anxrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/model"
)

func publicRegistration(t *testing.T, runner *fakeRunner) Registration {
	t.Helper()
	var body struct {
		Event struct {
			Payload struct {
				Text string `json:"text"`
			} `json:"payload"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(runner.stdins[len(runner.stdins)-1]), &body); err != nil {
		t.Fatal(err)
	}
	var got Registration
	if err := json.Unmarshal([]byte(body.Event.Payload.Text), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDisclosedOriginCoordinates(t *testing.T) {
	for _, host := range []string{"github.com", "forge.internal.ts.net", "100.101.102.103:3000", "[2001:db8::1]:3000"} {
		for _, name := range []string{"agentctl", "999.100.101.102.103", "pve3.tailnet.ts.net", strings.Repeat("repo-", 130), "dự án.wiki"} {
			for _, remote := range []string{"https://user:glpat-fixture@" + host + "/David_Zhang/" + strings.ReplaceAll(name, " ", "%20") + ".git", "ssh://user:glpat-fixture@" + host + "/David_Zhang/" + strings.ReplaceAll(name, " ", "%20") + ".git"} {
				origin := ParseOrigin(remote)
				if origin == nil || origin.Host != host || origin.Owner != "David_Zhang" || origin.Name != name {
					t.Fatalf("origin: %+v", origin)
				}
				r := testRegistration("exec-a", PhaseTerminal, 1)
				r.CWD = "/Users/david/workspace/" + name
				r.Outcome = &Outcome{Availability: AvailabilityStored, Links: []Link{{Kind: LinkKindPR, Owner: origin.Owner, Repo: name, Number: 104}}}
				policy := Verification{Origin: origin, Cards: map[string]ConfirmedCard{"card:watch-redesign": {Ref: "card:watch-redesign", ThreadID: "thread-test"}}}
				runner := &fakeRunner{}
				if err := (&CLISender{options: CLIOptions{Verification: &policy}, run: runner.run}).Send(context.Background(), r); err != nil {
					t.Fatal(err)
				}
				got := publicRegistration(t, runner)
				if got.Repository == nil || *got.Repository != *origin || got.CWD != r.CWD || len(got.Outcome.Links) != 1 {
					t.Fatalf("disclosure rejected: %+v", got)
				}
				for i, raw := range runner.stdins {
					if strings.Contains(raw+strings.Join(runner.argvs[i], " "), "glpat-fixture") {
						t.Fatal("userinfo escaped")
					}
				}
			}
		}
	}
	if got := ParseOrigin("user:glpat-fixture@forge.local:David_Zhang/repo.git"); got == nil || got.Host != "forge.local" || got.Name != "repo" {
		t.Fatalf("SCP userinfo: %+v", got)
	}
}

func TestRepositorySecretsDropOnlyMatchingComponent(t *testing.T) {
	for _, secret := range []string{"glpat-abcdefghijklmnopqrst", "cR8yLa3mK9xQ7vZ1bF6dT4wN2pJ5sU0h"} {
		for _, field := range []string{"host", "owner", "name", "cwd"} {
			t.Run(field+secret[:5], func(t *testing.T) {
				origin := &Repository{Host: "forge.local", Owner: "David_Zhang", Name: "repo"}
				r := testRegistration("exec-a", PhaseTerminal, 1)
				r.CWD = "/Users/david/workspace/agentctl"
				switch field {
				case "host":
					origin.Host = secret
				case "owner":
					origin.Owner = secret
				case "name":
					origin.Name = secret
				case "cwd":
					r.CWD = "/Users/" + secret + "/repo"
				}
				policy := Verification{Origin: origin, Cards: map[string]ConfirmedCard{"card:watch-redesign": {Ref: "card:watch-redesign", ThreadID: "thread-test"}}}
				runner := &fakeRunner{}
				if err := (&CLISender{options: CLIOptions{Verification: &policy}, run: runner.run}).Send(context.Background(), r); err != nil {
					t.Fatal(err)
				}
				got := publicRegistration(t, runner)
				if got.EgressErrorCode != EgressSecretShape || got.Repository == nil {
					t.Fatalf("lost registration: %+v", got)
				}
				expected := *origin
				switch field {
				case "host":
					expected.Host = ""
				case "owner":
					expected.Owner = ""
				case "name":
					expected.Name = ""
				case "cwd":
					r.CWD = ""
				}
				if *got.Repository != expected || got.CWD != r.CWD {
					t.Fatalf("unrelated fields dropped: %+v", got)
				}
				for i, raw := range runner.stdins {
					if strings.Contains(raw+strings.Join(runner.argvs[i], " "), secret) {
						t.Fatal("credential escaped")
					}
				}
			})
		}
	}
}

func TestRepositoryMetadataObeysOwnerRestriction(t *testing.T) {
	r := testRegistration("exec-a", PhaseStart, 1)
	for _, allowed := range [][]string{nil, {"david_zhang"}, {"other"}} {
		policy := Verification{Origin: &Repository{Host: "forge.local", Owner: "David_Zhang", Name: "repo"}, AllowedOrgs: allowed}
		got, err := Allowlisted(r, nil, policy)
		if err != nil {
			t.Fatal(err)
		}
		if (got.Repository != nil) != (len(allowed) == 0 || allowed[0] == "david_zhang") {
			t.Fatalf("owner policy: %+v", got)
		}
	}
}

func TestWorkingDirectoryFromBothAuthorities(t *testing.T) {
	for _, authority := range []model.Authority{model.AuthorityNative, model.AuthorityMultica} {
		execution := testExecution(t, []string{"anx.card.watch-redesign"}, model.StateRunning)
		execution.Authority = authority
		cwd := "/Users/david/workspace/agentctl"
		execution.CWD = &cwd
		r := BuildStart(execution, execution.CreatedAt)
		s := testSpool(t)
		entry, err := s.Enqueue(r)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Registration.CWD != cwd {
			t.Fatal("working directory lost in spool")
		}
	}
}
