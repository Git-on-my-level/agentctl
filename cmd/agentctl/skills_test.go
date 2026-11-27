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
	"github.com/Git-on-my-level/agentctl/internal/testutil"
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
	root := testutil.TempDir(t)
	hub := filepath.Join(root, "hub")
	if err := os.Mkdir(hub, 0o700); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, hub, "init", "--quiet", "-b", "main")
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
	gitFixtureCommand(t, author, "init", "--quiet", "-b", "main")
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
	root := testutil.TempDir(t)
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

func TestSkillsConflictDoesNotSuggestDriftOnlyDiff(t *testing.T) {
	configPath, home := configuredSkillSource(t)
	destination := filepath.Join(home, ".agents", "skills", "test-skill")
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".codex", "skills", "test-skill"), destination); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--config", configPath, "skills", "status", "--home", home}); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"state":"conflict"`) {
		t.Fatalf("status omitted conflict: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), `"argv":["agentctl","skills","diff"`) {
		t.Fatalf("conflict advertised drift-only diff: %s", stdout.String())
	}
}

func TestSkillsDriftedCheckoutIsRecordedAndDoctorStillListsSkills(t *testing.T) {
	configPath, home := configuredSkillSource(t)
	t.Setenv("HOME", home)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--config", configPath, "skills", "reconcile", "--home", home}); code != 0 {
		t.Fatalf("reconcile exit=%d output=%s", code, stdout.String())
	}
	checkout, statePath, err := skillpack.DefaultHubPaths(home)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		AppliedCommit string `json:"applied_commit"`
		Selection     struct {
			Remote string `json:"remote"`
		} `json:"selection"`
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	// Reproduce the 2026-11-18 incident: someone advances the derived cache
	// with plain git, so HEAD no longer matches the recorded applied commit.
	hub := state.Selection.Remote
	if err := os.WriteFile(filepath.Join(hub, "skills", "test-skill", "SKILL.md"), []byte("---\nname: test-skill\n---\n\n# Test v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, hub, "-c", "user.name=Agentctl Test", "-c", "user.email=agentctl@example.test", "commit", "--quiet", "-am", "advance")
	gitFixtureCommand(t, checkout, "fetch", "--quiet", "origin", "main")
	gitFixtureCommand(t, checkout, "checkout", "--quiet", "--detach", "FETCH_HEAD")
	// The daily worker runs on the next UTC day.
	tomorrow := time.Now().Add(24 * time.Hour)
	a.now = func() time.Time { return tomorrow }

	if _, err := a.updateSkillsAutoClean(context.Background(), common{configPath: configPath}, false); err == nil {
		t.Fatal("drifted checkout was updated")
	}
	attempt, err := skillpack.ReadUpdateAttempt(home)
	if err != nil || attempt == nil {
		t.Fatalf("worker failure not recorded: %v %v", attempt, err)
	}
	if attempt.Succeeded || attempt.Trigger != "auto" || attempt.ErrorCode != "conflict" || strings.Join(attempt.Drift, ",") != "checkout_revision" || attempt.ConsecutiveFailures != 1 || attempt.FirstFailedAt == nil {
		t.Fatalf("unexpected record: %+v", attempt)
	}
	if _, err := a.updateSkillsAutoClean(context.Background(), common{configPath: configPath}, false); err == nil {
		t.Fatal("drifted checkout was updated on retry")
	}
	if attempt, _ = skillpack.ReadUpdateAttempt(home); attempt == nil || attempt.ConsecutiveFailures != 2 {
		t.Fatalf("retry did not accumulate: %+v", attempt)
	}

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--config", configPath, "update", "status"}); code != 0 {
		t.Fatalf("update status exit=%d output=%s", code, stdout.String())
	}
	var status struct {
		Result struct {
			Skills struct {
				Healthy     bool                    `json:"healthy"`
				LastAttempt skillpack.UpdateAttempt `json:"last_attempt"`
			} `json:"skills"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Result.Skills.Healthy || status.Result.Skills.LastAttempt.Succeeded || status.Result.Skills.LastAttempt.ErrorCode != "conflict" {
		t.Fatalf("update status hid the failure: %s", stdout.String())
	}

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--config", configPath, "skills", "doctor", "--home", home}); code != 0 {
		t.Fatalf("doctor refused a drifted checkout: exit=%d output=%s", code, stdout.String())
	}
	var doctor struct {
		Result struct {
			HubDrift struct {
				Drift           []string `json:"drift"`
				SkillsEvaluated bool     `json:"skills_evaluated"`
				EvaluatedCommit string   `json:"evaluated_commit"`
			} `json:"hub_drift"`
			Report     skillpack.Report         `json:"report"`
			LastUpdate *skillpack.UpdateAttempt `json:"last_update"`
		} `json:"result"`
		NextActions []struct {
			Argv []string `json:"argv"`
		} `json:"next_actions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doctor); err != nil {
		t.Fatalf("%v: %s", err, stdout.String())
	}
	head := gitFixtureCommand(t, checkout, "rev-parse", "HEAD")
	if strings.Join(doctor.Result.HubDrift.Drift, ",") != "checkout_revision" || !doctor.Result.HubDrift.SkillsEvaluated || doctor.Result.HubDrift.EvaluatedCommit != head {
		t.Fatalf("doctor drift finding incomplete: %s", stdout.String())
	}
	if doctor.Result.Report.Healthy || len(doctor.Result.Report.Actions) != 1 || doctor.Result.Report.Actions[0].Skill != "test-skill" {
		t.Fatalf("doctor did not list skills: %s", stdout.String())
	}
	if doctor.Result.LastUpdate == nil || doctor.Result.LastUpdate.Succeeded {
		t.Fatalf("doctor omitted the failed update: %s", stdout.String())
	}
	repair := false
	for _, action := range doctor.NextActions {
		if strings.Join(action.Argv, " ") == "git -C "+checkout+" checkout --quiet --detach "+state.AppliedCommit {
			repair = true
		}
	}
	if !repair {
		t.Fatalf("doctor omitted the repair action: %s", stdout.String())
	}

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--config", configPath, "skills", "reconcile", "--home", home}); code == 0 || !strings.Contains(stdout.String(), "drifted Skill Hub checkout") {
		t.Fatalf("mutating reconcile accepted a drifted checkout: exit=%d output=%s", code, stdout.String())
	}

	gitFixtureCommand(t, checkout, "checkout", "--quiet", "--detach", state.AppliedCommit)
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--config", configPath, "skills", "update", "--home", home}); code != 0 {
		t.Fatalf("update after repair exit=%d output=%s", code, stdout.String())
	}
	attempt, err = skillpack.ReadUpdateAttempt(home)
	if err != nil || attempt == nil || !attempt.Succeeded || attempt.Trigger != "explicit" || attempt.ConsecutiveFailures != 0 || attempt.FirstFailedAt != nil || attempt.Error != "" {
		t.Fatalf("successful update did not clear the failure: %+v %v", attempt, err)
	}
}
