package main

import (
	"bytes"
	"context"
	"encoding/json"
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
				data := []byte("not valid json")
				mode := os.FileMode(0o600)
				if kind == "unsafe" {
					mode = 0o644
					data = []byte(`{"schema_version":1,"profiles":{}}`)
				}
				if err := os.WriteFile(path, data, mode); err != nil {
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

func TestUpdateNowHonorsExplicitMissingConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("AGENTCTL_CONFIG", "")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.getenv = os.Getenv
	if code := a.run(context.Background(), []string{"--config", filepath.Join(root, "explicit.json"), "update", "now"}); code == 0 {
		t.Fatalf("explicit missing config silently skipped: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "apply Skill Hub update") {
		t.Fatalf("wrong failure: %s", stdout.String())
	}
}

func TestUpdateStatusAndWorkerKeepExplicitConfigSelection(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("AGENTCTL_CONFIG", filepath.Join(root, "missing-environment.json"))
	selected := filepath.Join(root, "explicit with spaces.json")
	if err := os.WriteFile(selected, []byte(`{"schema_version":1,"profiles":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.getenv = os.Getenv
	if code := a.run(context.Background(), []string{"--config", selected, "update", "status"}); code != 0 {
		t.Fatalf("status=%d %s", code, stdout.String())
	}
	var envelope struct {
		Result struct{ Skills map[string]any }
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if err, present := envelope.Result.Skills["error"]; present {
		t.Fatalf("explicit config was lost: %v", err)
	}
	// Cross the actual child argv/parser boundary: a flag must still override an
	// inherited environment selection, including when its path contains spaces.
	command := updateWorkerCommand("agentctl", common{configPath: selected})
	parsed, rest, err := a.parseCommon(command.Args[1:])
	if err != nil || len(rest) != 1 || rest[0] != "_update-worker" || parsed.configPath != selected {
		t.Fatalf("worker selection lost: %#v %v %v", parsed, rest, err)
	}
	path, err := configPath(parsed)
	if err != nil || path != selected {
		t.Fatalf("worker path=%q err=%v", path, err)
	}
}
