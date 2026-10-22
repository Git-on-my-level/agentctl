package adapter

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
)

var readOnlyEnv = []string{"ZCODE_MODEL=glm-5.3-flash", launchrecipe.ZCodeReadOnlyProfileEnv}

func TestZCodeLaunchWithoutProfileWritesNothing(t *testing.T) {
	dir := t.TempDir()
	release, err := prepareZCodeLaunch(dir, []string{"ZCODE_MODEL=glm-5.3"})
	if err != nil || release != nil {
		t.Fatalf("release=%v err=%v", release != nil, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".zcode")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf(".zcode created without the profile: %v", err)
	}
}

func TestZCodeReadOnlyProfileIsWrittenAndRemoved(t *testing.T) {
	dir := t.TempDir()
	release, err := prepareZCodeLaunch(dir, readOnlyEnv)
	if err != nil || release == nil {
		t.Fatalf("release=%v err=%v", release != nil, err)
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
	if _, err := os.Lstat(filepath.Join(dir, ".zcode")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf(".zcode left behind: %v", err)
	}
}

func TestZCodeReadOnlyProfileKeepsExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, ".zcode", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := prepareZCodeLaunch(dir, readOnlyEnv)
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

func TestZCodeReadOnlyProfileRefusesConflicts(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"existing config": func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, ".zcode"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".zcode", "config.json"), []byte(`{"plugins":{"enabled":true}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"zcode.json": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "zcode.json"), []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked .zcode": func(t *testing.T, dir string) {
			target := t.TempDir()
			if err := os.Symlink(target, filepath.Join(dir, ".zcode")); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked config": func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, ".zcode"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.json"), filepath.Join(dir, ".zcode", "config.json")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			setup(t, dir)
			release, err := prepareZCodeLaunch(dir, readOnlyEnv)
			var adapterErr *AdapterError
			if release != nil || !errors.As(err, &adapterErr) || adapterErr.Code != ErrCapabilityUnavailable || adapterErr.Details["diagnostic_code"] != "zcode_read_only_config_conflict" {
				t.Fatalf("release=%v err=%v", release != nil, err)
			}
		})
	}
}

func TestZCodeReadOnlyReleaseKeepsReplacedConfig(t *testing.T) {
	dir := t.TempDir()
	release, err := prepareZCodeLaunch(dir, readOnlyEnv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".zcode", "config.json")
	if err := os.WriteFile(path, []byte(`{"edited":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
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

// The child must see the profile at start and the launch must remove it once
// the child is reaped.
func TestZCodeReadOnlyLaunchHoldsProfileForChildLifetime(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "zcode")
	script := "#!/bin/sh\nif [ -f .zcode/config.json ]; then seen=yes; else seen=no; fi\nprintf '{\"sessionId\":\"sess_1\",\"turnId\":\"turn_1\",\"response\":\"%s\",\"projection\":{\"status\":\"idle\"}}\\n' \"$seen\"\n"
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
	if _, err := os.Lstat(filepath.Join(dir, ".zcode")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("profile left after the child exited: %v", err)
	}
}
