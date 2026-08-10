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
