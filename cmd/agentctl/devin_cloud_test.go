package main

import "testing"

func TestGitHubRepositoryFromOrigin(t *testing.T) {
	for remote, want := range map[string]string{
		"git@github.com:Git-on-my-level/agentctl.git":  "Git-on-my-level/agentctl",
		"https://github.com/Git-on-my-level/agentctl":  "Git-on-my-level/agentctl",
		"https://user:token@github.com/owner/repo.git": "owner/repo",
		"ssh://git@github.com/owner/repo.git":          "owner/repo",
		"https://gitlab.com/owner/repo.git":            "",
		"https://github.com/owner":                     "",
		"https://github.com/owner/repo/extra":          "",
		"http://github.com/owner/repo":                 "",
		"/Users/me/src/repo":                           "",
		"git@github.com.evil.example:owner/repo.git":   "",
	} {
		got, ok := githubRepository(remote)
		if got != want || ok != (want != "") {
			t.Fatalf("%s: got %q %v", remote, got, ok)
		}
	}
}
