package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
	"github.com/Git-on-my-level/agentctl/internal/zcodeconfig"
)

// ZCodeReasonProjectConfigPresent identifies a read-only launch refused because
// project configuration could override the reviewed profile.
const ZCodeReasonProjectConfigPresent = "project_config_present"

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
var zcodeVersion = func(executable, dir string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--version")
	cmd.Dir = dir
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
// directory or its ancestors during the launch: such a writer can already
// change what any agent reads there.
func prepareZCodeLaunch(cwd string, argv, env []string) (func(), []string, error) {
	if !hasEnvEntry(env, launchrecipe.ZCodeReadOnlyProfileEnv) {
		return nil, nil, nil
	}
	refuse := func(reason string) error {
		reasonCode := "profile_unavailable"
		if strings.HasPrefix(reason, "a ZCode project config is present at ") {
			reasonCode = ZCodeReasonProjectConfigPresent
		}
		return &AdapterError{Code: ErrCapabilityUnavailable, Message: "zcode read_only profile refused: " + reason, Retryable: false,
			Details: map[string]any{"diagnostic_code": "zcode_read_only_unavailable", "reason_code": reasonCode, "cwd": cwd}}
	}
	if len(argv) < 3 || argv[len(argv)-2] != "--prompt" {
		return nil, nil, refuse("the prompt must be the final argv value")
	}
	// ZCode trims with JavaScript's String.prototype.trim (Unicode white space,
	// line terminators and the BOM) before matching a command. Strip a
	// superset: all Unicode white space and format characters.
	trimmed := strings.TrimLeftFunc(argv[len(argv)-1], func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) })
	if strings.HasPrefix(trimmed, "/") {
		return nil, nil, refuse("a prompt starting with / would expand a ZCode command")
	}
	if !filepath.IsAbs(argv[0]) {
		return nil, nil, refuse("the ZCode executable must be an absolute path")
	}
	if !filepath.IsAbs(cwd) {
		return nil, nil, refuse("working directory is not absolute")
	}
	// Node reports the physical directory, so every check below must see the
	// same path ZCode will: refuse a working directory reached through a
	// symlink rather than guess which ancestors ZCode will search.
	if physical, err := filepath.EvalSymlinks(cwd); err != nil || physical != filepath.Clean(cwd) {
		return nil, nil, refuse("working directory must be a canonical path without symlinks")
	}
	if info, err := os.Lstat(cwd); err != nil || !info.IsDir() {
		return nil, nil, refuse("working directory is not a directory")
	}
	// Project configs merge from the repository root down and can define hooks
	// and MCP servers; ZCode also loads the nearest .env upward, which can
	// redirect its storage and provider settings. Refuse any of them.
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		for _, name := range []string{"zcode.json", filepath.Join(".zcode", "config.json"), ".env"} {
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
	provider := zcodeconfig.ProviderPath(realHome, envValue(env, "ZCODE_MODEL"), "")
	if provider == "" {
		return nil, nil, refuse("unsupported ZCODE_MODEL")
	}
	if info, err := os.Stat(provider); err != nil || !info.Mode().IsRegular() {
		return nil, nil, refuse("provider config missing; run zcode-cli-provision")
	}
	home, err := os.MkdirTemp("", "agentctl-zcode-ro-home-")
	if err != nil {
		return nil, nil, refuse("cannot create an isolated home: " + err.Error())
	}
	removeHome := func() { _ = os.RemoveAll(home) }
	extra, err := zcodeReadOnlyEnv(env, home, provider)
	if err != nil {
		removeHome()
		return nil, nil, refuse(err.Error())
	}
	version, err := zcodeVersion(argv[0], cwd, mergeEnvironment(append([]string(nil), env...), extra))
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
	syncErr := file.Sync()
	created, statErr := file.Stat()
	if statErr != nil {
		writeErr = statErr
	}
	// The descriptor stays open until release. While it is open the inode
	// cannot be freed, so no later file can reuse its number and pass the
	// identity check (filesystems such as ext4 and tmpfs reuse inodes at once).
	release := func() {
		if created != nil {
			removeIfOurs(path, created, zcodeReadOnlyConfig)
		}
		_ = file.Close()
		if createdDir {
			// Fails, and so keeps the directory, if anything else was added.
			_ = os.Remove(dir)
		}
		removeHome()
	}
	if writeErr != nil || syncErr != nil {
		release()
		return nil, nil, refuse("cannot write .zcode/config.json")
	}
	return release, extra, nil
}

// removeIfOurs removes the profile only while path still names the very file
// this launch created, unchanged. It first renames the entry to an unused
// random sidecar, so the entry it checks is the entry it removes; anything else
// found there is put back if the path is still free, or left under the
// sidecar name, never deleted. It never opens a non-regular file, so a FIFO
// swapped in cannot block it.
// zcodeReadOnlyEnv builds the child's environment overrides. ZCode reads a
// wide set of ZCODE_* and OTEL_* variables (storage, logging, mailbox,
// telemetry export, app and provider locations), so inheritance is deny by
// default: every inherited variable with either prefix is blanked unless set
// here. Storage, logs and the mailbox live inside the isolated home;
// messaging and telemetry are off. The provider config is copied into the
// home, because ZCode may rewrite the file it is given while reading it.
func zcodeReadOnlyEnv(env []string, home, provider string) ([]string, error) {
	zdir := filepath.Join(home, ".zcode")
	copied := filepath.Join(zdir, "cli", "provider_config.json")
	if err := copyPrivate(provider, copied); err != nil {
		return nil, errors.New("cannot copy the provider config into the isolated home")
	}
	db := filepath.Join(zdir, "cli", "db", "db.sqlite")
	tmp := filepath.Join(home, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, errors.New("cannot create the isolated temporary directory")
	}
	set := map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_DATA_HOME": filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME": filepath.Join(home, ".local", "state"), "XDG_CACHE_HOME": filepath.Join(home, ".cache"),
		"ZCODE_HOME": zdir, "ZCODE_STORAGE_DIR": zdir, "ZCODE_DATA_BASE_DIR": zdir, "ZCODE_SESSION_DB_PATH": db, "ZCODE_SESSION_DB": db,
		"ZCODE_LOG_DIR": filepath.Join(zdir, "logs"), "ZCODE_MAILBOX_ROOT": filepath.Join(zdir, "mailbox"), "ZCODE_MESSAGE_ENABLED": "0",
		"ZCODE_MODEL_TELEMETRY_ENABLED": "0", "OTEL_SDK_DISABLED": "true", "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE": copied,
		// Read hands PDFs to Poppler (pdfinfo, pdftoppm) found on PATH, which
		// writes temporary files. A system-only PATH leaves no Poppler to run
		// (ZCode bundles the ripgrep its search tools need), and any temporary
		// file still lands inside the isolated home.
		"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR": tmp, "TMP": tmp, "TEMP": tmp,
		// ZCode runs fixed, read-only git queries at startup to describe the
		// working directory. Keep them from writing the index, reading user or
		// system config, prompting, or running an fsmonitor hook from repo config.
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_OPTIONAL_LOCKS": "0", "GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.fsmonitor", "GIT_CONFIG_VALUE_0": "false",
	}
	// agentctl's own activity source binds the log directory it watches; keep
	// that one, and only that one.
	if dir := envValue(env, "ZCODE_LOG_DIR"); agentctlActivityLogDir(dir) {
		set["ZCODE_LOG_DIR"] = dir
	}
	extra := make([]string, 0, len(set)+8)
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !zcodeReadOnlyBlankedKey(key) {
			continue
		}
		if _, pinned := set[key]; !pinned {
			extra = append(extra, key+"=")
		}
	}
	for key, value := range set {
		extra = append(extra, key+"="+value)
	}
	return extra, nil
}

// zcodeReadOnlyBlankedKey reports the inherited variables a read-only child
// must not see unless agentctl sets them: ZCode's own and OpenTelemetry
// settings, and the Node, Electron and dynamic-loader variables that can load
// code into ZCode's Electron-as-Node process before any control applies.
func zcodeReadOnlyBlankedKey(key string) bool {
	for _, prefix := range []string{"ZCODE_", "OTEL_", "NODE_", "ELECTRON_", "DYLD_", "LD_", "npm_config_", "NPM_CONFIG_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// agentctlActivityLogDir reports whether dir is the log directory this
// process's own activity source created for a launch. A caller-supplied
// directory, however it is named, is never trusted.
func agentctlActivityLogDir(dir string) bool {
	if dir == "" {
		return false
	}
	_, ok := zcodeActivityDirs.Load(filepath.Clean(dir))
	return ok
}

func copyPrivate(from, to string) error {
	source, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer source.Close()
	if info, err := source.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return errors.New("provider config is not a small regular file")
	}
	data, err := io.ReadAll(io.LimitReader(source, 1<<20+1))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := target.Write(data); err != nil {
		_ = target.Close()
		return err
	}
	return target.Close()
}

func removeIfOurs(path string, created fs.FileInfo, want []byte) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return
	}
	aside := path + ".agentctl-release-" + hex.EncodeToString(nonce[:])
	if _, err := os.Lstat(aside); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err := os.Rename(path, aside); err != nil {
		return
	}
	matches := false
	if info, err := os.Lstat(aside); err == nil && info.Mode().IsRegular() && os.SameFile(info, created) {
		if file, err := os.OpenFile(aside, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0); err == nil {
			opened, statErr := file.Stat()
			got, readErr := io.ReadAll(io.LimitReader(file, int64(len(want))+1))
			_ = file.Close()
			matches = statErr == nil && readErr == nil && os.SameFile(opened, created) && bytes.Equal(got, want)
		}
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
