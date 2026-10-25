package adapter

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
)

var readOnlyEnv = []string{"ZCODE_MODEL=glm-5.3-flash", launchrecipe.ZCodeReadOnlyProfileEnv}

func readOnlyArgv(prompt string) []string {
	return []string{"/fake/zcode", "--json", "--mode", "plan", "--prompt", prompt}
}

func TestZCodeReadOnlyReleaseKeepsSameBytesDifferentFile(t *testing.T) {
	withReadOnlyHost(t, "0.17.0")
	dir := canonicalTemp(t)
	release, _, err := prepareZCodeLaunch(dir, readOnlyArgv("hi"), readOnlyEnv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".zcode", "config.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, string(zcodeReadOnlyConfig))
	sidecar := path + ".agentctl-release"
	mustWrite(t, sidecar, "someone else's")
	release()
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, zcodeReadOnlyConfig) {
		t.Fatalf("a different file with identical bytes was removed: %v", err)
	}
	if got, err := os.ReadFile(sidecar); err != nil || string(got) != "someone else's" {
		t.Fatalf("an existing sidecar was overwritten: %q %v", got, err)
	}
}

func TestZCodeReadOnlyPinsStorageInsideIsolatedHome(t *testing.T) {
	withReadOnlyHost(t, "0.17.0")
	inherited := []string{"ZCODE_STORAGE_DIR=/tmp/outside", "ZCODE_SESSION_DB_PATH=/tmp/outside.db", "ZCODE_LOG_DIR=/tmp/outside-logs",
		"ZCODE_MAILBOX_ROOT=/tmp/outside-mail", "ZCODE_MESSAGE_ENABLED=1", "ZCODE_APP=/tmp/Other.app", "ZCODE_BUILTIN_PROVIDER_CONFIG_FILE=/tmp/evil.json",
		"ZCODE_SOMETHING_NEW=x", "OTEL_EXPORTER_OTLP_ENDPOINT=https://telemetry.invalid", "OTEL_EXPORTER_OTLP_HEADERS=k=v",
		"NODE_OPTIONS=--require /tmp/hook.js", "ELECTRON_EXTRA=1", "DYLD_INSERT_LIBRARIES=/tmp/x.dylib", "LD_PRELOAD=/tmp/x.so"}
	release, extra, err := prepareZCodeLaunch(canonicalTemp(t), readOnlyArgv("hi"), append(inherited, readOnlyEnv...))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	home := envValue(extra, "HOME")
	if got := envValue(extra, "PATH"); got != "/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Fatalf("PATH=%q", got)
	}
	for key, want := range map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_OPTIONAL_LOCKS": "0", "GIT_CONFIG_KEY_0": "core.fsmonitor", "GIT_CONFIG_VALUE_0": "false"} {
		if got := envValue(extra, key); got != want {
			t.Fatalf("%s=%q want %q", key, got, want)
		}
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP", "ZCODE_HOME", "ZCODE_STORAGE_DIR", "ZCODE_DATA_BASE_DIR", "ZCODE_SESSION_DB_PATH", "ZCODE_SESSION_DB", "ZCODE_LOG_DIR", "ZCODE_MAILBOX_ROOT", "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if value := envValue(extra, key); !strings.HasPrefix(value, home+string(filepath.Separator)) {
			t.Fatalf("%s=%q is outside the isolated home %q", key, value, home)
		}
	}
	child := mergeEnvironment(append(inherited, readOnlyEnv...), extra)
	for key, want := range map[string]string{"ZCODE_MESSAGE_ENABLED": "0", "ZCODE_MODEL_TELEMETRY_ENABLED": "0", "OTEL_SDK_DISABLED": "true",
		"ZCODE_APP": "", "ZCODE_BUILTIN_PROVIDER_CONFIG_FILE": "", "ZCODE_SOMETHING_NEW": "", "ZCODE_MODEL": "",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "", "OTEL_EXPORTER_OTLP_HEADERS": "", "NODE_OPTIONS": "", "ELECTRON_EXTRA": "",
		"DYLD_INSERT_LIBRARIES": "", "LD_PRELOAD": ""} {
		if got := envValue(child, key); got != want {
			t.Fatalf("child %s=%q want %q", key, got, want)
		}
	}
}

func TestZCodeReadOnlyKeepsOnlyAgentctlActivityLogDir(t *testing.T) {
	withReadOnlyHost(t, "0.17.0")
	source, bound, err := newZCodeActivity(nil)
	if err != nil || source == nil {
		t.Fatalf("source=%v err=%v", source, err)
	}
	owned := envValue(bound, "ZCODE_LOG_DIR")
	defer os.RemoveAll(owned)
	forged, err := os.MkdirTemp("", "agentctl-zcode-log-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(forged)
	release, extra, err := prepareZCodeLaunch(canonicalTemp(t), readOnlyArgv("hi"), append([]string{"ZCODE_LOG_DIR=" + forged}, readOnlyEnv...))
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(extra, "ZCODE_LOG_DIR"); got == forged {
		t.Fatalf("a caller-created directory was trusted: %q", got)
	}
	release()
	release, extra, err = prepareZCodeLaunch(canonicalTemp(t), readOnlyArgv("hi"), append([]string{"ZCODE_LOG_DIR=" + owned}, readOnlyEnv...))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := envValue(extra, "ZCODE_LOG_DIR"); got != owned {
		t.Fatalf("activity log dir replaced: %q", got)
	}
}

// withReadOnlyHost gives the test a fake real home holding provisioned
// provider configs and a ZCode that reports version.
func withReadOnlyHost(t *testing.T, version string) {
	t.Helper()
	home := t.TempDir()
	for dir, content := range map[string]string{"": `{}`, filepath.Join("models", "glm-5.3-flash"): `{"model":"flash"}`} {
		path := filepath.Join(home, ".zcode", "cli", dir, "provider_config.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	previous := zcodeVersion
	zcodeVersion = func(string, string, []string) (string, error) { return version, nil }
	t.Cleanup(func() { zcodeVersion = previous })
}

func refusal(t *testing.T, release func(), err error) {
	t.Helper()
	var adapterErr *AdapterError
	if release != nil || !errors.As(err, &adapterErr) || adapterErr.Code != ErrCapabilityUnavailable || adapterErr.Details["diagnostic_code"] != "zcode_read_only_unavailable" {
		t.Fatalf("release=%v err=%v", release != nil, err)
	}
}

func TestZCodeLaunchWithoutProfileWritesNothing(t *testing.T) {
	dir := t.TempDir()
	release, extra, err := prepareZCodeLaunch(dir, readOnlyArgv("hi"), []string{"ZCODE_MODEL=glm-5.3"})
	if err != nil || release != nil || extra != nil {
		t.Fatalf("release=%v extra=%v err=%v", release != nil, extra, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".zcode")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf(".zcode created without the profile: %v", err)
	}
}

func TestZCodeReadOnlyIsolatesHomeAndWritesProfile(t *testing.T) {
	withReadOnlyHost(t, "0.17.0")
	realHome := os.Getenv("HOME")
	dir := canonicalTemp(t)
	release, extra, err := prepareZCodeLaunch(dir, readOnlyArgv("judge these trees"), readOnlyEnv)
	if err != nil || release == nil {
		t.Fatalf("release=%v err=%v", release != nil, err)
	}
	isolated := envValue(extra, "HOME")
	if isolated == "" || isolated == realHome || envValue(extra, "ZCODE_MODEL") != "" || !hasEnvEntry(extra, "ZCODE_MODEL=") {
		t.Fatalf("extra=%v", extra)
	}
	copied := envValue(extra, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE")
	if copied != filepath.Join(isolated, ".zcode", "cli", "provider_config.json") {
		t.Fatalf("provider=%q", copied)
	}
	if got, err := os.ReadFile(copied); err != nil || string(got) != `{"model":"flash"}` {
		t.Fatalf("copied provider=%q err=%v", got, err)
	}
	if info, _ := os.Lstat(copied); info.Mode().Perm() != 0o600 {
		t.Fatalf("copied provider mode=%v", info.Mode())
	}
	if original, _ := os.ReadFile(filepath.Join(realHome, ".zcode", "cli", "models", "glm-5.3-flash", "provider_config.json")); string(original) != `{"model":"flash"}` {
		t.Fatalf("real provider changed: %q", original)
	}
	path := filepath.Join(dir, ".zcode", "config.json")
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, zcodeReadOnlyConfig) {
		t.Fatalf("config=%q err=%v", got, err)
	}
	if info, _ := os.Lstat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v", info.Mode())
	}
	release()
	for _, gone := range []string{filepath.Join(dir, ".zcode"), isolated} {
		if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s left behind: %v", gone, err)
		}
	}
}

func TestZCodeReadOnlyKeepsExistingDirectory(t *testing.T) {
	withReadOnlyHost(t, "0.17.0")
	dir := canonicalTemp(t)
	keep := filepath.Join(dir, ".zcode", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, _, err := prepareZCodeLaunch(dir, readOnlyArgv("hi"), readOnlyEnv)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("existing .zcode content removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".zcode", "config.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("config left behind: %v", err)
	}
}

func TestZCodeReadOnlyRefusals(t *testing.T) {
	cases := map[string]struct {
		version  string
		prompt   string
		setup    func(t *testing.T, dir string)
		symlink  bool
		relative bool
	}{
		"command prompt":   {prompt: "  /judge"},
		"nbsp command":     {prompt: "\u00a0/judge"},
		"bom command":      {prompt: "\ufeff/judge"},
		"line sep command": {prompt: "\u2028/judge\nargs"},
		"dotenv in ancestor": {setup: func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(filepath.Dir(dir), ".env"), "ZCODE_STORAGE_DIR=/tmp/x")
		}},
		"symlinked cwd":       {setup: func(t *testing.T, dir string) {}, symlink: true},
		"relative executable": {relative: true},
		"unreviewed version":  {version: "0.18.0"},
		"existing config": {setup: func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(dir, ".zcode", "config.json"), `{"plugins":{"enabled":true}}`)
		}},
		"zcode.json": {setup: func(t *testing.T, dir string) { mustWrite(t, filepath.Join(dir, "zcode.json"), `{}`) }},
		"ancestor config": {setup: func(t *testing.T, dir string) {
			mustWrite(t, filepath.Join(filepath.Dir(dir), ".zcode", "config.json"), `{"hooks":{}}`)
		}},
		"symlinked .zcode": {setup: func(t *testing.T, dir string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".zcode")); err != nil {
				t.Fatal(err)
			}
		}},
		"symlinked config": {setup: func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, ".zcode"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.json"), filepath.Join(dir, ".zcode", "config.json")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			version := tc.version
			if version == "" {
				version = "0.17.0"
			}
			withReadOnlyHost(t, version)
			dir := filepath.Join(canonicalTemp(t), "work")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			prompt := tc.prompt
			if prompt == "" {
				prompt = "hi"
			}
			if tc.symlink {
				link := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				dir = link
			}
			argv := readOnlyArgv(prompt)
			if tc.relative {
				argv[0] = "./bin/zcode"
			}
			release, _, err := prepareZCodeLaunch(dir, argv, readOnlyEnv)
			refusal(t, release, err)
		})
	}
}

func TestZCodeReadOnlyReleaseKeepsReplacedConfig(t *testing.T) {
	withReadOnlyHost(t, "0.17.0")
	dir := canonicalTemp(t)
	release, _, err := prepareZCodeLaunch(dir, readOnlyArgv("hi"), readOnlyEnv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".zcode", "config.json")
	mustWrite(t, path, `{"edited":true}`)
	release()
	if got, err := os.ReadFile(path); err != nil || string(got) != `{"edited":true}` {
		t.Fatalf("replaced config removed or changed: %q %v", got, err)
	}
}

func TestZCodeAdapterDeclaresReadOnlyPreparation(t *testing.T) {
	z, ok := NewZCode().(*zcodeAdapter)
	if !ok || z.config.PrepareLaunch == nil {
		t.Fatal("zcode adapter does not prepare read_only launches")
	}
}

// The child must start with the profile and the isolated home, and the launch
// must remove both once the child is reaped.
func TestZCodeReadOnlyLaunchHoldsProfileForChildLifetime(t *testing.T) {
	withReadOnlyHost(t, "0.17.0")
	dir := canonicalTemp(t)
	marker := filepath.Join(t.TempDir(), "home")
	bin := filepath.Join(t.TempDir(), "zcode")
	script := "#!/bin/sh\nprintf '%s' \"$HOME\" > " + marker + "\nif [ -f .zcode/config.json ]; then seen=yes; else seen=no; fi\nprintf '{\"sessionId\":\"sess_1\",\"turnId\":\"turn_1\",\"response\":\"%s\",\"projection\":{\"status\":\"idle\"}}\\n' \"$seen\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := NewZCode().Launch(context.Background(), LaunchRequest{Argv: []string{bin, "--json", "--mode", "plan", "--prompt", "x"}, Cwd: dir, Env: readOnlyEnv, PromptDelivery: "argv", PromptArgs: 1, DiscoveryWindow: time.Second, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result == nil || result.Result.Content != "yes" {
		t.Fatalf("child did not see the profile: %#v", result.Result)
	}
	childHome, _ := os.ReadFile(marker)
	if len(childHome) == 0 || string(childHome) == os.Getenv("HOME") || !strings.Contains(string(childHome), "agentctl-zcode-ro-home-") {
		t.Fatalf("child HOME=%q", childHome)
	}
	for _, gone := range []string{filepath.Join(dir, ".zcode"), string(childHome)} {
		if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s left after the child exited: %v", gone, err)
		}
	}
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestZCodeReadOnlyProjectConfigReason(t *testing.T) {
	dir := canonicalTemp(t)
	mustWrite(t, filepath.Join(dir, "zcode.json"), `{}`)
	release, _, err := prepareZCodeLaunch(dir, readOnlyArgv("hi"), readOnlyEnv)
	refusal(t, release, err)
	var ae *AdapterError
	if !errors.As(err, &ae) || ae.Details["reason_code"] != ZCodeReasonProjectConfigPresent {
		t.Fatalf("project config reason: %+v", err)
	}
	if ZCodeReasonProjectConfigPresent != "project_config_present" {
		t.Fatal("project config reason wire value changed")
	}
}
