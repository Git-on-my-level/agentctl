package adapter

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
)

// zcodeReadOnlyConfig is the reviewed project config a read_only ZCode launch
// runs under. ZCode CLI 0.17.0 has no per-launch flag that removes plugin MCP
// tools (its --disallowed-tools matches whole names only, and the JavaScript
// REPL arrives as a plugin MCP tool). Its working-directory project config is
// the one per-launch control that does: it turns plugins, MCP, subagents,
// skills and memory off. Plan mode and the recipe's tool denylist cover the
// built-in tools.
var zcodeReadOnlyConfig = []byte(`{"features":{"mcp":false,"subagent":false,"skill":false,"memory":false},"plugins":{"enabled":false}}` + "\n")

// prepareZCodeLaunch lays down the read-only project config for launches the
// read_only recipe marked, and does nothing for any other launch. It never
// overwrites or adopts a config it did not write: an existing project config,
// a symlinked .zcode, or a zcode.json beside it refuses the launch, because
// any of them could re-enable a tool the profile removes.
func prepareZCodeLaunch(cwd string, env []string) (func(), error) {
	if !hasEnvEntry(env, launchrecipe.ZCodeReadOnlyProfileEnv) {
		return nil, nil
	}
	refuse := func(reason string) error {
		return &AdapterError{Code: ErrCapabilityUnavailable, Message: "zcode read_only profile refused: " + reason, Retryable: false,
			Details: map[string]any{"diagnostic_code": "zcode_read_only_config_conflict", "cwd": cwd}}
	}
	if !filepath.IsAbs(cwd) {
		return nil, refuse("working directory is not absolute")
	}
	if info, err := os.Lstat(cwd); err != nil || !info.IsDir() {
		return nil, refuse("working directory is not a directory")
	}
	if _, err := os.Lstat(filepath.Join(cwd, "zcode.json")); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil, refuse("a zcode.json project config is present")
	}
	dir := filepath.Join(cwd, ".zcode")
	createdDir := false
	switch info, err := os.Lstat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, refuse("cannot create .zcode: " + err.Error())
		}
		createdDir = true
	case err != nil:
		return nil, refuse("cannot inspect .zcode: " + err.Error())
	case !info.IsDir():
		return nil, refuse(".zcode is not a real directory")
	}
	path := filepath.Join(dir, "config.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if createdDir {
			_ = os.Remove(dir)
		}
		if errors.Is(err, fs.ErrExist) {
			return nil, refuse(".zcode/config.json already exists")
		}
		return nil, refuse("cannot create .zcode/config.json: " + err.Error())
	}
	_, writeErr := file.Write(zcodeReadOnlyConfig)
	closeErr := file.Close()
	release := func() {
		removeIfExactly(path, zcodeReadOnlyConfig)
		if createdDir {
			// Fails, and so keeps the directory, if anything else was added.
			_ = os.Remove(dir)
		}
	}
	if writeErr != nil || closeErr != nil {
		release()
		return nil, refuse("cannot write .zcode/config.json")
	}
	return release, nil
}

// removeIfExactly deletes path only while it is still a regular file holding
// exactly want, so a release never removes a file someone else replaced.
func removeIfExactly(path string, want []byte) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	info, statErr := file.Stat()
	got, readErr := io.ReadAll(io.LimitReader(file, int64(len(want))+1))
	_ = file.Close()
	if statErr != nil || readErr != nil || !info.Mode().IsRegular() || !bytes.Equal(got, want) {
		return
	}
	_ = os.Remove(path)
}

func hasEnvEntry(env []string, entry string) bool {
	for _, value := range env {
		if value == entry {
			return true
		}
	}
	return false
}
