package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/skillpack"
)

func gitFixtureCommand(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func configuredSkillSource(t *testing.T) (configPath, home string) {
	t.Helper()
	root := t.TempDir()
	hub := filepath.Join(root, "hub")
	if err := os.Mkdir(hub, 0o700); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, hub, "init", "--quiet")
	hubFiles := map[string]string{
		"manifests/fleet-core.json":  `{"schema_version":1,"skills":[{"name":"test-skill","path":"skills/test-skill","targets":["codex"]}]}`,
		"skills/test-skill/SKILL.md": "---\nname: test-skill\n---\n\n# Test\n",
	}
	for relative, content := range hubFiles {
		path := filepath.Join(hub, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitFixtureCommand(t, hub, "add", ".")
	gitFixtureCommand(t, hub, "-c", "user.name=Agentctl Test", "-c", "user.email=agentctl@example.test", "commit", "--quiet", "-m", "hub fixture")
	author := filepath.Join(root, "author")
	if err := os.Mkdir(author, 0o700); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, author, "init", "--quiet")
	files := map[string]string{
		"config-bundle.json": `{"schema_version":1,"default_profile":"local","profiles":{"local":{"adapters":{"codex":{"executable":"/bin/echo"}}}},"skills":{"source":{"remote":` + string(mustJSON(t, hub)) + `,"ref":"main","manifest_path":"manifests/fleet-core.json"},"update_policy":"auto-clean"}}`,
	}
	for relative, content := range files {
		path := filepath.Join(author, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitFixtureCommand(t, author, "add", ".")
	gitFixtureCommand(t, author, "-c", "user.name=Agentctl Test", "-c", "user.email=agentctl@example.test", "commit", "--quiet", "-m", "fixture")

	configPath = filepath.Join(root, "runtime", "config.json")
	checkout := filepath.Join(root, "managed")
	if _, err := config.InitSource(context.Background(), configPath, config.SourceSpec{Remote: author, CheckoutPath: checkout}, time.Now()); err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := skillpack.UpdateHubSource(context.Background(), home, skillpack.HubSelection{Remote: hub, Ref: "main", ManifestPath: "manifests/fleet-core.json"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return configPath, home
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSkillsPlanIsReadOnlyAndReconcileUsesPinnedConfigSource(t *testing.T) {
	configPath, home := configuredSkillSource(t)
	destination := filepath.Join(home, ".agents", "skills", "test-skill", "SKILL.md")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--config", configPath, "skills", "plan", "--home", home}); code != 0 {
		t.Fatalf("plan exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"state":"install"`) || !strings.Contains(stdout.String(), `"mutates":true`) {
		t.Fatalf("plan omitted install or next action: %s", stdout.String())
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("read-only plan installed content: %v", err)
	}

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--config", configPath, "skills", "reconcile", "--home", home}); code != 0 {
		t.Fatalf("reconcile exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"applied":1`) || !strings.Contains(stdout.String(), `"state":"current"`) {
		t.Fatalf("reconcile omitted applied/current state: %s", stdout.String())
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsRequiresDurableGitSourceAndRejectsInvocationBundle(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "missing", "config.json")
	if err := config.Save(configPath, config.Config{SchemaVersion: config.SchemaVersion}, false); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--config", configPath, "skills", "plan", "--home", home}); code != 5 || !strings.Contains(stdout.String(), `"code":"capability_unavailable"`) {
		t.Fatalf("missing source exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--config", configPath, "--config-bundle", filepath.Join(root, "bundle.json"), "skills", "plan", "--home", home}); code != 2 || !strings.Contains(stdout.String(), `"code":"usage"`) {
		t.Fatalf("invocation bundle exit=%d output=%s", code, stdout.String())
	}
}
