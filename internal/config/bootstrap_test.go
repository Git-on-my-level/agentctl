package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testBootstrapConfig() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Profiles: map[string]Profile{
			"local": {Adapters: map[string]Adapter{"codex": {Executable: "/bin/echo"}}},
		},
	}
}

func TestSaveCreatesOwnerOnlyConfigAndParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "agentctl", "config.json")
	if err := Save(path, testBootstrapConfig(), false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != ConfigFileMode {
		t.Fatalf("mode = %04o, want %04o", got, ConfigFileMode)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("saved config does not load: %v", err)
	}
}

func TestSaveManagedConflictAndExplicitOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first := testBootstrapConfig()
	if err := Save(path, first, false); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, first, false); err != nil {
		t.Fatalf("same managed config should be idempotent: %v", err)
	}
	second := testBootstrapConfig()
	second.Profiles["local"] = Profile{Adapters: map[string]Adapter{"cursor": {Executable: "/bin/echo"}}}
	if err := Save(path, second, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if err := Save(path, second, true); err != nil {
		t.Fatalf("overwrite failed: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Profiles["local"].Adapters["cursor"]; !ok {
		t.Fatalf("overwrite did not install replacement profile: %#v", loaded)
	}
}

func TestSaveRejectsUnmanagedTrailingJSONAndModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1}{"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, testBootstrapConfig(), false); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("trailing JSON error = %v, want ErrUnmanaged", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, testBootstrapConfig(), false); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("broad mode error = %v, want ErrUnmanaged", err)
	}
	if err := Save(path, testBootstrapConfig(), true); err != nil {
		t.Fatalf("explicit overwrite failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != ConfigFileMode {
		t.Fatalf("mode after overwrite = %04o, want %04o", got, ConfigFileMode)
	}
}

func TestSaveRejectsSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := Save(filepath.Join(link, "config.json"), testBootstrapConfig(), true); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink component error = %v, want ErrUnsafePath", err)
	}
	final := filepath.Join(root, "final.json")
	if err := os.Symlink(target, final); err != nil {
		t.Fatal(err)
	}
	if err := Save(final, testBootstrapConfig(), true); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink destination error = %v, want ErrUnsafePath", err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Fatalf("symlink target changed: %q", content)
	}
}

func TestBuildProfileRequiresCompleteExplicitMulticaAndNoFalseDefault(t *testing.T) {
	if _, err := BuildProfile(ProfileInput{}); err == nil {
		t.Fatal("empty profile input unexpectedly accepted")
	}
	if _, err := BuildProfile(ProfileInput{MulticaExecutable: "/bin/echo", MulticaProfile: "profile", WorkspaceID: "workspace", ServerURL: "https://server"}); err == nil {
		t.Fatal("missing app_url unexpectedly accepted")
	}
	if _, err := BuildProfile(ProfileInput{Multica: &Multica{Executable: "/bin/echo", Profile: "p", WorkspaceID: "w", ServerURL: "https://u:p@server", AppURL: "https://app"}}); err == nil {
		t.Fatal("credential-bearing URL unexpectedly accepted")
	}
	profile, err := BuildProfile(ProfileInput{AdapterName: "codex", AdapterExecutable: "/bin/echo"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := MergeProfile(Config{}, "local", profile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProfile != "" {
		t.Fatalf("profile merge selected false default %q", cfg.DefaultProfile)
	}
}

func TestCheckProvenanceIsLocalAndStructured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adapter")
	if err := os.WriteFile(path, []byte("adapter binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := Profile{Adapters: map[string]Adapter{"local": {Executable: path}}, Multica: &Multica{Executable: path, Profile: "p", WorkspaceID: "w", ServerURL: "https://server", AppURL: "https://app"}}
	report, err := CheckConfigProvenance(Config{SchemaVersion: SchemaVersion, DefaultProfile: "p", Profiles: map[string]Profile{"p": profile}}, "", ProvenanceOptions{ResolveExecutable: func(string) (string, error) { return path, nil }})
	if err != nil {
		t.Fatalf("provenance failed: %v (%#v)", err, report)
	}
	if !report.Valid || report.Multica == nil || report.Multica.Digest == "" {
		t.Fatalf("incomplete provenance report: %#v", report)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Doctor(ctx, Config{}, ""); err == nil {
		t.Fatal("cancelled doctor unexpectedly succeeded")
	}
}
