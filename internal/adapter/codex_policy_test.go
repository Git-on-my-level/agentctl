package adapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCodexDelegateAllowlistIsNarrowAndGeneratedAsRules(t *testing.T) {
	rules := string(codexDelegateRules())
	for _, command := range [][]string{{"git", "status", "--short"}, {"go", "test", "./..."}, {"npm", "run", "build"}, {"cargo", "test"}} {
		if !matchesCodexDelegatePrefix(command) {
			t.Fatalf("expected allowlisted command %v", command)
		}
		if !strings.Contains(rules, "pattern=[\""+command[0]+"\", \""+command[1]+"\"") {
			t.Fatalf("generated policy missing prefix for %v: %s", command, rules)
		}
	}
	for _, command := range [][]string{{"git", "push"}, {"git", "clean", "-fd"}, {"curl", "https://example.test"}, {"sudo", "make", "test"}} {
		if matchesCodexDelegatePrefix(command) {
			t.Fatalf("outside command unexpectedly allowlisted: %v", command)
		}
	}
	if strings.Contains(rules, "pattern=[\"git\", \"push\"") || strings.Contains(rules, "pattern=[\"curl\"") {
		t.Fatalf("generated policy widened beyond reviewed commands: %s", rules)
	}
}

func TestCodexDelegateLaunchUsesIsolatedHomeAndKeepsUserRules(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "checkout")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	rulesDir := filepath.Join(base, "rules")
	if err := os.Mkdir(rulesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := []byte("model = \"configured-model\"\n")
	config = append(config, codexProjectTrustConfig(t, repo, "trusted")...)
	if err := os.WriteFile(filepath.Join(base, "config.toml"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	userRules := []byte("prefix_rule(pattern=[\"git\", \"push\"], decision=\"forbidden\")\n")
	if err := os.WriteFile(filepath.Join(rulesDir, "default.rules"), userRules, 0o600); err != nil {
		t.Fatal(err)
	}
	operatorRules := []byte("prefix_rule(pattern=[\"git\", \"status\"], decision=\"forbidden\")\n")
	if err := os.WriteFile(filepath.Join(rulesDir, "agentctl-delegate.rules"), operatorRules, 0o600); err != nil {
		t.Fatal(err)
	}
	release, extra, err := prepareCodexDelegateLaunch(repo, nil, []string{"CODEX_HOME=" + base, codexDelegateAllowlistEnv + "=1"})
	if err != nil {
		t.Fatal(err)
	}
	if release == nil {
		t.Fatal("launch-scoped home did not return cleanup")
	}
	defer release()
	overlay := envValue(extra, "CODEX_HOME")
	if overlay == "" || overlay == base {
		t.Fatalf("CODEX_HOME overlay=%q base=%q", overlay, base)
	}
	if got, err := os.ReadFile(filepath.Join(overlay, "config.toml")); err != nil || string(got) != string(config) {
		t.Fatalf("config.toml through overlay = %q, err=%v", got, err)
	}
	if info, err := os.Lstat(filepath.Join(overlay, "config.toml")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("user config was not linked into overlay: info=%v err=%v", info, err)
	}
	gotUserRules, err := os.ReadFile(filepath.Join(overlay, "rules", "default.rules"))
	if err != nil || string(gotUserRules) != string(userRules) {
		t.Fatalf("user rules were not preserved: %q, err=%v", gotUserRules, err)
	}
	gotOperatorRules, err := os.ReadFile(filepath.Join(overlay, "rules", "agentctl-delegate.rules"))
	if err != nil || string(gotOperatorRules) != string(operatorRules) {
		t.Fatalf("same-named operator rules were overwritten: %q, err=%v", gotOperatorRules, err)
	}
	entries, err := os.ReadDir(filepath.Join(overlay, "rules"))
	if err != nil {
		t.Fatal(err)
	}
	var generatedPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "agentctl-delegate-generated-") && strings.HasSuffix(entry.Name(), ".rules") {
			generatedPath = filepath.Join(overlay, "rules", entry.Name())
			break
		}
	}
	if generatedPath == "" {
		t.Fatalf("unique generated policy missing: %v", entries)
	}
	generatedRules, err := os.ReadFile(generatedPath)
	if err != nil || !strings.Contains(string(generatedRules), `pattern=["git", "status"]`) {
		t.Fatalf("generated policy contents=%q err=%v", generatedRules, err)
	}
	baseOperatorRules, err := os.ReadFile(filepath.Join(rulesDir, "agentctl-delegate.rules"))
	if err != nil || string(baseOperatorRules) != string(operatorRules) {
		t.Fatalf("launch modified persistent user rules: %q, err=%v", baseOperatorRules, err)
	}
}

func TestCodexFreshHomeSessionsPersistForResume(t *testing.T) {
	base := filepath.Join(t.TempDir(), "fresh-codex-home")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(base, "config.toml"), codexProjectTrustConfig(t, repo, "trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "sessions")); !os.IsNotExist(err) {
		t.Fatalf("fixture sessions directory already exists: %v", err)
	}
	firstRelease, firstEnv, err := prepareCodexDelegateLaunch(repo, nil, []string{"CODEX_HOME=" + base, codexDelegateAllowlistEnv + "=1"})
	if err != nil {
		t.Fatal(err)
	}
	firstOverlay := envValue(firstEnv, "CODEX_HOME")
	if firstOverlay == "" {
		t.Fatal("first launch has no CODEX_HOME overlay")
	}
	if info, err := os.Lstat(filepath.Join(firstOverlay, "sessions")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("fresh sessions directory was not linked to persistent home: info=%v err=%v", info, err)
	}
	rollout := filepath.Join(firstOverlay, "sessions", "thread-fixture", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollout, []byte("first-session-rollout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstRelease()
	if got, err := os.ReadFile(filepath.Join(base, "sessions", "thread-fixture", "rollout.jsonl")); err != nil || string(got) != "first-session-rollout\n" {
		t.Fatalf("first rollout did not survive overlay cleanup: %q err=%v", got, err)
	}
	secondRelease, secondEnv, err := prepareCodexDelegateLaunch(repo, nil, []string{"CODEX_HOME=" + base, codexDelegateAllowlistEnv + "=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer secondRelease()
	resumedRollout := filepath.Join(envValue(secondEnv, "CODEX_HOME"), "sessions", "thread-fixture", "rollout.jsonl")
	if got, err := os.ReadFile(resumedRollout); err != nil || string(got) != "first-session-rollout\n" {
		t.Fatalf("follow-up launch cannot read first session rollout: %q err=%v", got, err)
	}
}

func TestCodexDelegateLaunchRequiresExplicitTrustedCheckout(t *testing.T) {
	for _, test := range []struct {
		name       string
		trustLevel string
		trusted    bool
	}{
		{name: "trusted checkout", trustLevel: "trusted", trusted: true},
		{name: "untrusted checkout", trustLevel: "untrusted"},
		{name: "default checkout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "checkout")
			if err := os.Mkdir(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			home := filepath.Join(root, "codex-home")
			if err := os.Mkdir(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if test.trustLevel != "" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), codexProjectTrustConfig(t, repo, test.trustLevel), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			release, extra, err := prepareCodexDelegateLaunch(repo, nil, []string{"CODEX_HOME=" + home, codexDelegateAllowlistEnv + "=1"})
			if err != nil {
				t.Fatal(err)
			}
			if test.trusted {
				if release == nil {
					t.Fatal("explicitly trusted checkout did not receive delegated rules")
				}
				defer release()
				overlay := envValue(extra, "CODEX_HOME")
				if overlay == "" || overlay == home {
					t.Fatalf("trusted checkout did not receive isolated Codex home: %v", extra)
				}
				entries, err := os.ReadDir(filepath.Join(overlay, "rules"))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) == 0 {
					t.Fatal("trusted checkout has no generated allow rules")
				}
				return
			}
			if release != nil || len(extra) != 0 {
				t.Fatalf("untrusted/default checkout received bypass rules: release=%v env=%v", release != nil, extra)
			}
			if _, err := os.Stat(filepath.Join(home, "sessions")); !os.IsNotExist(err) {
				t.Fatalf("untrusted/default checkout prepared launch state: %v", err)
			}
		})
	}
}

func TestCodexDelegateLaunchWithoutGrantDoesNotPreparePolicy(t *testing.T) {
	base := filepath.Join(t.TempDir(), "codex-home")
	release, extra, err := prepareCodexDelegateLaunch("", nil, []string{"CODEX_HOME=" + base})
	if err != nil || release != nil || len(extra) != 0 {
		t.Fatalf("ungranted launch changed Codex home: has_release=%v extra=%v err=%v", release != nil, extra, err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("ungranted launch created Codex home: %v", err)
	}
}

func matchesCodexDelegatePrefix(command []string) bool {
	for _, prefix := range codexDelegateCommandPrefixes {
		if len(command) < len(prefix) {
			continue
		}
		match := true
		for i := range prefix {
			if command[i] != prefix[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func codexProjectTrustConfig(t *testing.T, repo, level string) []byte {
	t.Helper()
	root, err := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("resolve Git root: %v", err)
	}
	return []byte("[projects." + strconv.Quote(strings.TrimSpace(string(root))) + "]\ntrust_level = " + strconv.Quote(level) + "\n")
}
