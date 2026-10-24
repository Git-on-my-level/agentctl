package anxrun

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryOriginAndOptionalAllowlist(t *testing.T) {
	origin := &Repository{Owner: "Git-on-my-level", Name: "agentctl"}
	for name, policy := range map[string]Verification{
		"empty":          {Origin: origin},
		"other-org":      {Origin: origin, AllowedOrgs: []string{"other"}},
		"no-origin":      {AllowedOrgs: []string{"Git-on-my-level"}},
		"other-repo":     {Origin: &Repository{Owner: origin.Owner, Name: "other"}, AllowedOrgs: []string{origin.Owner}},
		"allowed-origin": {Origin: origin, AllowedOrgs: []string{"git-on-my-level"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := testRegistration("exec-a", PhaseTerminal, 1)
			r.Outcome = &Outcome{Availability: AvailabilityStored, Links: []Link{{Kind: LinkKindPR, Owner: origin.Owner, Repo: origin.Name, Number: 104}}}
			policy.Cards = map[string]ConfirmedCard{"card:watch-redesign": {Ref: "card:watch-redesign", ThreadID: "thread-test"}}
			runner := &fakeRunner{}
			sender := &CLISender{options: CLIOptions{Verification: &policy}, run: runner.run}
			if err := sender.Send(context.Background(), r); err != nil {
				t.Fatal(err)
			}
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
			if (len(got.Outcome.Links) == 1) != (name == "allowed-origin" || name == "empty") {
				t.Fatal("repository authority bypass")
			}
		})
	}
}

func TestReview4OriginIgnoresGitEnvironment(t *testing.T) {
	makeRepo := func(name string) string {
		t.Helper()
		root := t.TempDir()
		for _, argv := range [][]string{{"init", "--quiet", root}, {"-C", root, "config", "--local", "remote.origin.url", "https://github.com/Git-on-my-level/" + name + ".git"}} {
			if out, err := exec.Command("git", argv...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, out)
			}
		}
		return root
	}
	wanted, foreign := makeRepo("agentctl"), makeRepo("foreign")
	for name, environment := range map[string]map[string]string{
		"repository": {"GIT_DIR": filepath.Join(foreign, ".git"), "GIT_WORK_TREE": foreign, "GIT_COMMON_DIR": filepath.Join(foreign, ".git"), "GIT_INDEX_FILE": filepath.Join(foreign, ".git", "index")},
		"config":     {"GIT_CONFIG": filepath.Join(foreign, ".git", "config"), "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "remote.origin.url", "GIT_CONFIG_VALUE_0": "https://github.com/Git-on-my-level/foreign.git"},
		"discovery":  {"GIT_CEILING_DIRECTORIES": wanted, "GIT_CONFIG_PARAMETERS": "'remote.origin.url=https://github.com/Git-on-my-level/foreign.git'"},
	} {
		t.Run(name, func(t *testing.T) {
			for key, value := range environment {
				t.Setenv(key, value)
			}
			got := OriginRepository(context.Background(), wanted)
			if got == nil || got.Owner != "Git-on-my-level" || got.Name != "agentctl" {
				t.Fatal("inherited Git environment changed run origin")
			}
		})
	}
}

func TestReview4WorkspaceAliasGrammar(t *testing.T) {
	for _, value := range []string{"anx", "work-space-2"} {
		if !ValidWorkspaceAlias(value) {
			t.Fatal("valid alias rejected")
		}
	}
	for _, value := range []string{"", "ANX", "anx/work", "anx_space", "-anx", strings.Repeat("a", 65), "glpat-abcdefghijklmnopqrst", strings.Repeat("a", 38) + "glpat-abcdefghijklmnopqrst"} {
		if ValidWorkspaceAlias(value) {
			t.Fatal("unsafe alias accepted")
		}
		runner := &fakeRunner{}
		sender := &CLISender{options: CLIOptions{WorkspaceAlias: value}, run: runner.run}
		// Empty aliases deliberately normalize to the default for delivery.
		if value != "" {
			if _, err := sender.invokeResult(context.Background(), "cards.list", nil, nil); err == nil || len(runner.argvs) != 0 {
				t.Fatal("unsafe alias reached subprocess")
			}
		}
	}
}
