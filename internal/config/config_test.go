package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestLoadAndResolveExplicitMulticaProfile(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "config.json")
	data := `{
  "schema_version": 1,
  "default_profile": "fleet",
  "profiles": {
    "fleet": {
      "multica": {
        "executable": "/opt/agent/bin/multica",
        "profile": "desktop-private.example",
        "workspace_id": "workspace-authority-id",
        "server_url": "https://multica.internal.example",
        "app_url": "https://multica.internal.example"
      }
    }
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	name, profile, err := cfg.ResolveProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if name != "fleet" || profile.Multica.Profile != "desktop-private.example" {
		t.Fatalf("unexpected resolution: %q %#v", name, profile)
	}
}

func TestBootstrapInstructionPointersPolicy(t *testing.T) {
	if err := (Bootstrap{InstructionPointers: "off"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Bootstrap{InstructionPointers: "manage"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Bootstrap{InstructionPointers: "silent"}).Validate(); err == nil {
		t.Fatal("invalid instruction_pointers was accepted")
	}
	dir := testutil.TempDir(t)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	path := filepath.Join(dir, "config.json")
	cfg := testBootstrapConfig()
	cfg.Bootstrap = &Bootstrap{InstructionPointers: "off"}
	if err := Save(path, cfg, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Bootstrap == nil || loaded.Bootstrap.InstructionPointers != "off" {
		t.Fatalf("bootstrap policy was not round-tripped: %#v %v", loaded.Bootstrap, err)
	}
}

func TestLoadDoesNotCreateMissingConfig(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "missing.json")
	if _, err := Load(path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("load created or changed missing path: %v", err)
	}
}

func TestLoadReportsValidationFailuresSeparatelyFromFileSecurity(t *testing.T) {
	root := testutil.TempDir(t)
	oversized := filepath.Join(root, "oversized.json")
	if err := os.WriteFile(oversized, []byte(`{"schema_version":1,"profiles":{"local":{"agent_preferences":{"mode":"advisory","preferred":[{"agent":"cursor","model":"example"}],"notes":["first","second","`+strings.Repeat("x", 1025)+`"]}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(oversized); err == nil || !strings.Contains(err.Error(), "config invalid:") || !strings.Contains(err.Error(), "notes[2] exceeds 1024-byte limit") {
		t.Fatalf("oversized note error=%v", err)
	}
	if err := Save(oversized, testBootstrapConfig(), false); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("write replaced invalid config without explicit overwrite: %v", err)
	}

	malformed := filepath.Join(root, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`{"schema_version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(malformed); err == nil || !strings.Contains(err.Error(), "config invalid: parse config:") {
		t.Fatalf("malformed JSON error=%v", err)
	}

	insecure := filepath.Join(root, "insecure.json")
	if err := os.WriteFile(insecure, []byte(`{"schema_version":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(insecure); !errors.Is(err, ErrUnmanaged) || !strings.Contains(err.Error(), "regular owner-only 0600") || strings.Contains(err.Error(), "config invalid") {
		t.Fatalf("0644 error=%v", err)
	}

	target := filepath.Join(root, "symlink-target.json")
	if err := os.WriteFile(target, []byte(`{"schema_version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "symlink.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Load(link); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink error=%v", err)
	}
}

func TestMulticaRequiresExactProfileWorkspaceAndAppOrigin(t *testing.T) {
	cases := []Multica{
		{Executable: "multica", WorkspaceID: "ws", ServerURL: "https://server", AppURL: "https://app"},
		{Executable: "multica", Profile: "p", ServerURL: "https://server", AppURL: "https://app"},
		{Executable: "multica", Profile: "p", WorkspaceID: "ws", ServerURL: "https://server"},
		{Executable: "multica", Profile: "p", WorkspaceID: "ws", ServerURL: "https://u:p@server", AppURL: "https://app"},
	}
	for i, value := range cases {
		if err := value.Validate(); err == nil {
			t.Fatalf("case %d unexpectedly valid: %#v", i, value)
		}
	}
}

func TestRunLinksValidation(t *testing.T) {
	valid := []RunLinks{
		{},
		{Default: "labeled"},
		{Default: "all"},
		{Default: "off"},
		{WorkspaceAlias: "anx"},
		{BaseURL: "https://anx.example.test"},
		{BaseURL: "http://127.0.0.1:8080"},
		{SummaryMaxBytes: 512},
		{FlushTimeoutSecs: 30},
		{SpoolDir: "/var/lib/agentctl/anx"},
	}
	for i, value := range valid {
		if err := value.Validate(); err != nil {
			t.Fatalf("valid case %d rejected: %#v (%v)", i, value, err)
		}
	}
	invalid := []RunLinks{
		{Default: "sometimes"},
		{WorkspaceAlias: "Anx"},
		{WorkspaceAlias: "has space"},
		{BaseURL: "not a url"},
		{BaseURL: "https://u:p@anx.example.test"},
		{BaseURL: "http://anx.example.test"},
		{BaseURL: "https://anx.example.test/?token=1"},
		{SummaryMaxBytes: 4096},
		{FlushTimeoutSecs: 61},
		{Executable: "line\nbreak"},
	}
	for i, value := range invalid {
		if err := value.Validate(); err == nil {
			t.Fatalf("invalid case %d accepted: %#v", i, value)
		}
	}
}
