package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndResolveExplicitMulticaProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
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
	dir := t.TempDir()
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
	path := filepath.Join(t.TempDir(), "missing.json")
	if _, err := Load(path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("load created or changed missing path: %v", err)
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
