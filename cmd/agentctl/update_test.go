package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateNowWithoutOptionalConfigDoesNotFailOrCreateConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("AGENTCTL_CONFIG", "")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.getenv = os.Getenv
	// The test binary has a development version, so release discovery is skipped;
	// this exercises the real command's optional skill phase without networking.
	if code := a.run(context.Background(), []string{"update", "now"}); code != 0 {
		t.Fatalf("exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"ok":true`) || !strings.Contains(stdout.String(), `"updated":false`) {
		t.Fatalf("unexpected result: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, "config")); !os.IsNotExist(err) {
		t.Fatalf("optional config was created or inaccessible: %v", err)
	}
}

func TestAutomaticSkillsPreserveExplicitMissingAndMalformedConfigFailures(t *testing.T) {
	for _, kind := range []string{"flag-missing", "environment-missing", "malformed", "unsafe"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", root)
			t.Setenv("AGENTCTL_CONFIG", "")
			path := filepath.Join(root, "agentctl", "config.json")
			c := common{}
			switch kind {
			case "flag-missing":
				c.configPath = path
			case "environment-missing":
				t.Setenv("AGENTCTL_CONFIG", path)
			default:
				if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0o600)
				if kind == "unsafe" {
					mode = 0o644
				}
				if err := os.WriteFile(path, []byte("not valid json"), mode); err != nil {
					t.Fatal(err)
				}
			}
			a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
			if _, err := a.updateSkillsAutoClean(context.Background(), c, true); err == nil {
				t.Fatal("invalid configured selection was silently skipped")
			}
		})
	}
}
