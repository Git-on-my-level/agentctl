package adapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
)

// zcodeReadOnlyVersions are the ZCode CLI versions whose read-only surface was
// reviewed. A new version can add a tool or change config semantics, so any
// other version refuses rather than inherit the read_only label.
var zcodeReadOnlyVersions = map[string]bool{"0.17.0": true}

// zcodeReadOnlyConfig is the reviewed project config a read_only ZCode launch
// runs under. ZCode CLI 0.17.0 has no per-launch flag that removes plugin MCP
// tools (its --disallowed-tools matches whole names only, and the JavaScript
// REPL arrives as a plugin MCP tool), and it seeds its official plugins even
// into an empty HOME. Its working-directory project config is the one
// per-launch control that turns plugins, MCP, subagents, skills and memory off.
var zcodeReadOnlyConfig = []byte(`{"features":{"mcp":false,"subagent":false,"skill":false,"memory":false},"plugins":{"enabled":false},"hooks":{"enabled":false}}` + "\n")

// zcodeVersion is replaceable in tests; it reports the CLI version the given
// executable runs under env.
var zcodeVersion = func(executable string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--version")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// prepareZCodeLaunch isolates launches the read_only recipe marked and does
// nothing for any other launch. ZCode's tool flags alone do not make it
// read-only, so this layers every control 0.17.0 offers and refuses whenever
// one cannot be established:
//
//   - The prompt may not start with "/": ZCode expands a leading /name from
//     command files, which can run shell lines before the model is involved.
//   - Only a reviewed CLI version runs.
//   - No zcode.json or .zcode/config.json may exist in the working directory
//     or any ancestor: project configs merge from the repository root down and
//     can define hooks and MCP servers.
//   - The child gets a private, empty HOME, so user-level hooks, commands,
//     plugins and settings never load. The provider config the host launcher
//     would have chosen is passed by absolute path instead.
//   - The reviewed profile is created in the working directory with
//     no-clobber, no-follow semantics and removed after the child is reaped.
//
// It does not defend against a process that concurrently rewrites the working
// directory: such a writer can already change what any agent reads there.
func prepareZCodeLaunch(cwd string, argv, env []string) (func(), []string, error) {
	if !hasEnvEntry(env, launchrecipe.ZCodeReadOnlyProfileEnv) {
		return nil, nil, nil
	}
	refuse := func(reason string) error {
		return &AdapterError{Code: ErrCapabilityUnavailable, Message: "zcode read_only profile refused: " + reason, Retryable: false,
			Details: map[string]any{"diagnostic_code": "zcode_read_only_unavailable", "cwd": cwd}}
	}
	if len(argv) < 3 || argv[len(argv)-2] != "--prompt" {
		return nil, nil, refuse("the prompt must be the final argv value")
	}
	if strings.HasPrefix(strings.TrimLeft(argv[len(argv)-1], " \t\r\n"), "/") {
		return nil, nil, refuse("a prompt starting with / would expand a ZCode command")
	}
	if !filepath.IsAbs(cwd) {
		return nil, nil, refuse("working directory is not absolute")
	}
	if info, err := os.Lstat(cwd); err != nil || !info.IsDir() {
		return nil, nil, refuse("working directory is not a directory")
	}
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		for _, name := range []string{"zcode.json", filepath.Join(".zcode", "config.json")} {
			if _, err := os.Lstat(filepath.Join(dir, name)); err == nil || !errors.Is(err, fs.ErrNotExist) {
				return nil, nil, refuse("a ZCode project config is present at " + filepath.Join(dir, name))
			}
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, refuse("cannot resolve the user home")
	}
	provider := filepath.Join(realHome, ".zcode", "cli", "provider_config.json")
	if model := envValue(env, "ZCODE_MODEL"); model != "" {
		if model != "glm-5.3" && model != "glm-5.3-flash" {
			return nil, nil, refuse("unsupported ZCODE_MODEL")
		}
		provider = filepath.Join(realHome, ".zcode", "cli", "models", model, "provider_config.json")
	}
	if info, err := os.Stat(provider); err != nil || !info.Mode().IsRegular() {
		return nil, nil, refuse("provider config missing; run zcode-cli-provision")
	}
	home, err := os.MkdirTemp("", "agentctl-zcode-ro-home-")
	if err != nil {
		return nil, nil, refuse("cannot create an isolated home: " + err.Error())
	}
	extra := []string{"HOME=" + home, "ZCODE_MODEL=", "ZCODE_HOME=", "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=" + provider}
	removeHome := func() { _ = os.RemoveAll(home) }
	version, err := zcodeVersion(argv[0], mergeEnvironment(env, extra))
	if err != nil || !zcodeReadOnlyVersions[version] {
		removeHome()
		return nil, nil, refuse("ZCode CLI version " + strconvQuote(version) + " is not a reviewed read_only version")
	}
	dir := filepath.Join(cwd, ".zcode")
	createdDir := false
	switch info, err := os.Lstat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dir, 0o700); err != nil {
			removeHome()
			return nil, nil, refuse("cannot create .zcode: " + err.Error())
		}
		createdDir = true
	case err != nil:
		removeHome()
		return nil, nil, refuse("cannot inspect .zcode: " + err.Error())
	case !info.IsDir():
		removeHome()
		return nil, nil, refuse(".zcode is not a real directory")
	}
	path := filepath.Join(dir, "config.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if createdDir {
			_ = os.Remove(dir)
		}
		removeHome()
		return nil, nil, refuse("cannot create .zcode/config.json: " + err.Error())
	}
	_, writeErr := file.Write(zcodeReadOnlyConfig)
	closeErr := file.Close()
	release := func() {
		removeIfExactly(path, zcodeReadOnlyConfig)
		if createdDir {
			// Fails, and so keeps the directory, if anything else was added.
			_ = os.Remove(dir)
		}
		removeHome()
	}
	if writeErr != nil || closeErr != nil {
		release()
		return nil, nil, refuse("cannot write .zcode/config.json")
	}
	return release, extra, nil
}

// removeIfExactly removes the profile at path. It first renames the entry to a
// private name, so the bytes it checks are the bytes it removes; anything else
// found there is put back if the path is still free, or left under the
// private name, never deleted.
func removeIfExactly(path string, want []byte) {
	aside := path + ".agentctl-release"
	if err := os.Rename(path, aside); err != nil {
		return
	}
	file, err := os.OpenFile(aside, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	matches := false
	if err == nil {
		info, statErr := file.Stat()
		got, readErr := io.ReadAll(io.LimitReader(file, int64(len(want))+1))
		_ = file.Close()
		matches = statErr == nil && readErr == nil && info.Mode().IsRegular() && bytes.Equal(got, want)
	}
	if matches {
		_ = os.Remove(aside)
		return
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		_ = os.Rename(aside, path)
	}
}

func hasEnvEntry(env []string, entry string) bool {
	for _, value := range env {
		if value == entry {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			value = v
		}
	}
	return value
}

func strconvQuote(value string) string {
	if len(value) > 64 {
		value = value[:64]
	}
	return "\"" + value + "\""
}
