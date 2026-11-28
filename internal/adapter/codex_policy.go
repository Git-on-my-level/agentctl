package adapter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const codexDelegateAllowlistEnv = "AGENTCTL_CODEX_DELEGATE_ALLOWLIST"

var codexDelegateCommandPrefixes = [][]string{
	{"git", "status"}, {"git", "diff"}, {"git", "log"}, {"git", "show"},
	{"git", "rev-parse"}, {"git", "branch"}, {"git", "ls-files"}, {"git", "grep"},
	{"git", "add"}, {"git", "commit"},
	{"go", "build"}, {"go", "test"}, {"go", "vet"}, {"go", "fmt"},
	{"make", "build"}, {"make", "test"},
	{"npm", "test"}, {"npm", "run", "test"}, {"npm", "run", "build"},
	{"pnpm", "test"}, {"pnpm", "run", "test"}, {"pnpm", "run", "build"},
	{"yarn", "test"}, {"yarn", "run", "test"}, {"yarn", "run", "build"},
	{"bun", "test"}, {"bun", "run", "test"}, {"bun", "run", "build"},
	{"cargo", "build"}, {"cargo", "test"}, {"cargo", "fmt"}, {"cargo", "clippy"},
	{"pytest"}, {"python", "-m", "pytest"}, {"python3", "-m", "pytest"},
	{"python", "-m", "unittest"}, {"python3", "-m", "unittest"},
}

func prepareCodexDelegateLaunch(cwd string, _ []string, env []string) (func(), []string, error) {
	if envValue(env, codexDelegateAllowlistEnv) != "1" {
		return nil, nil, nil
	}
	baseHome := envValue(env, "CODEX_HOME")
	if baseHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("resolve Codex home: %w", err)
		}
		baseHome = filepath.Join(home, ".codex")
	}
	baseHome, err := filepath.Abs(baseHome)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve Codex home: %w", err)
	}
	if !codexCheckoutIsTrusted(baseHome, cwd) {
		// Codex `allow` rules bypass its sandbox. A profile-level unattended
		// permission grant must not extend that bypass to an untrusted or
		// unrecognized checkout.
		return nil, nil, nil
	}
	// Codex creates rollout files under sessions during a fresh exec. The
	// launch overlay links existing home entries, so create this persistent
	// directory before linking it or the first session would be deleted with
	// the overlay at process exit.
	if err := os.MkdirAll(filepath.Join(baseHome, "sessions"), 0o700); err != nil {
		return nil, nil, fmt.Errorf("prepare persistent Codex sessions: %w", err)
	}

	overlay, err := os.MkdirTemp("", "agentctl-codex-home-")
	if err != nil {
		return nil, nil, fmt.Errorf("create launch-scoped Codex home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(overlay) }
	if err := os.Chmod(overlay, 0o700); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("secure launch-scoped Codex home: %w", err)
	}
	if err := linkCodexHomeEntries(baseHome, overlay); err != nil {
		cleanup()
		return nil, nil, err
	}
	rulesDir := filepath.Join(overlay, "rules")
	if err := os.Mkdir(rulesDir, 0o700); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("create launch-scoped Codex rules: %w", err)
	}
	if err := copyCodexUserRules(filepath.Join(baseHome, "rules"), rulesDir); err != nil {
		cleanup()
		return nil, nil, err
	}
	rulesFile, err := os.CreateTemp(rulesDir, "agentctl-delegate-generated-*.rules")
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("create launch-scoped Codex command allowlist: %w", err)
	}
	rulesPath := rulesFile.Name()
	if _, err := rulesFile.Write(codexDelegateRules()); err != nil {
		_ = rulesFile.Close()
		_ = os.Remove(rulesPath)
		cleanup()
		return nil, nil, fmt.Errorf("write launch-scoped Codex command allowlist: %w", err)
	}
	if err := rulesFile.Close(); err != nil {
		_ = os.Remove(rulesPath)
		cleanup()
		return nil, nil, fmt.Errorf("close launch-scoped Codex command allowlist: %w", err)
	}
	return cleanup, []string{"CODEX_HOME=" + overlay}, nil
}

func codexCheckoutIsTrusted(codexHome, cwd string) bool {
	root, ok := codexGitProjectRoot(cwd)
	if !ok {
		return false
	}
	var config struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if _, err := toml.DecodeFile(filepath.Join(codexHome, "config.toml"), &config); err != nil {
		return false
	}
	trusted := false
	for configuredRoot, project := range config.Projects {
		if !filepath.IsAbs(configuredRoot) {
			continue
		}
		configuredRoot, err := canonicalDirectory(configuredRoot)
		if err != nil || configuredRoot != root {
			continue
		}
		// Conflicting entries through symlink aliases fail closed.
		if project.TrustLevel != "trusted" {
			return false
		}
		trusted = true
	}
	return trusted
}

func codexGitProjectRoot(cwd string) (string, bool) {
	root, err := canonicalDirectory(cwd)
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--show-toplevel")
	cmd.Env = codexGitEnvironment(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	projectRoot, err := canonicalDirectory(strings.TrimRight(string(out), "\r\n"))
	if err != nil {
		return "", false
	}
	return projectRoot, true
}

func canonicalDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "\x00\r\n") {
		return "", fmt.Errorf("invalid directory")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func codexGitEnvironment(env []string) []string {
	filtered := make([]string, 0, len(env)+3)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_OPTIONAL_LOCKS=0")
}

func linkCodexHomeEntries(source, overlay string) error {
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Codex home: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Codex home is not a directory")
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read Codex home: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == "rules" {
			continue
		}
		if err := os.Symlink(filepath.Join(source, entry.Name()), filepath.Join(overlay, entry.Name())); err != nil {
			return fmt.Errorf("link Codex home entry: %w", err)
		}
	}
	return nil
}

func copyCodexUserRules(source, destination string) error {
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Codex user rules: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Codex rules path is not a directory")
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read Codex user rules: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".rules") || entry.IsDir() {
			continue
		}
		path := filepath.Join(source, entry.Name())
		fileInfo, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect Codex rule file: %w", err)
		}
		if !fileInfo.Mode().IsRegular() {
			return fmt.Errorf("Codex rule file is not regular")
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read Codex rule file: %w", err)
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), contents, 0o600); err != nil {
			return fmt.Errorf("copy Codex rule file: %w", err)
		}
	}
	return nil
}

func codexDelegateRules() []byte {
	var out strings.Builder
	for _, prefix := range codexDelegateCommandPrefixes {
		quoted := make([]string, len(prefix))
		for i, token := range prefix {
			quoted[i] = strconv.Quote(token)
		}
		fmt.Fprintf(&out, "prefix_rule(pattern=[%s], decision=\"allow\", justification=\"Granted delegated build, test, and local Git command.\")\n", strings.Join(quoted, ", "))
	}
	return []byte(out.String())
}
