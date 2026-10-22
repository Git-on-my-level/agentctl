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

// withReadOnlyHost gives the test a fake real home holding provisioned
// provider configs and a ZCode that reports version.
func withReadOnlyHost(t *testing.T, version string) {
	t.Helper()
	home := t.TempDir()
	for _, dir := range []string{"", filepath.Join("models", "glm-5.3-flash")} {
		path := filepath.Join(home, ".zcode", "cli", dir, "provider_config.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	previous := zcodeVersion
	zcodeVersion = func(string, []string) (string, error) { return version, nil }
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
	dir := t.TempDir()
	release, extra, err := prepareZCodeLaunch(dir, readOnlyArgv("judge these trees"), readOnlyEnv)
	if err != nil || release == nil {
		t.Fatalf("release=%v err=%v", release != nil, err)
	}
	isolated := envValue(extra, "HOME")
	if isolated == "" || isolated == realHome || envValue(extra, "ZCODE_MODEL") != "" || !hasEnvEntry(extra, "ZCODE_MODEL=") {
		t.Fatalf("extra=%v", extra)
	}
	if got := envValue(extra, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE"); got != filepath.Join(realHome, ".zcode", "cli", "models", "glm-5.3-flash", "provider_config.json") {
		t.Fatalf("provider=%q", got)
	}
	if entries, err := os.ReadDir(isolated); err != nil || len(entries) != 0 {
		t.Fatalf("isolated home not empty: %v %v", entries, err)
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
	dir := t.TempDir()
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
		version string
		prompt  string
		setup   func(t *testing.T, dir string)
	}{
		"command prompt":     {prompt: "  /judge"},
		"unreviewed version": {version: "0.18.0"},
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
			dir := filepath.Join(t.TempDir(), "work")
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
			release, _, err := prepareZCodeLaunch(dir, readOnlyArgv(prompt), readOnlyEnv)
			refusal(t, release, err)
		})
	}
}

func TestZCodeReadOnlyReleaseKeepsReplacedConfig(t *testing.T) {
	withReadOnlyHost(t, "0.17.0")
	dir := t.TempDir()
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
	dir := t.TempDir()
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

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
