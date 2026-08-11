package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/portableasset"
)

func TestBootstrapStatusDetectsDuplicateAndReconciledHarnesses(t *testing.T) {
	originalVersion := version
	version = "v0.1.4"
	t.Cleanup(func() { version = originalVersion })
	home := t.TempDir()
	writeBootstrapSkill(t, filepath.Join(home, ".agents", "skills"), "tree:v0.1.4", "portable")
	writeBootstrapSkill(t, filepath.Join(home, ".codex", "skills"), "tree:v0.1.4", "portable")
	writeExecutable(t, filepath.Join(home, ".local", "bin", "agentctl"))

	status := buildBootstrapStatus(home, []string{"codex", "omp"}, func(string) string { return "" })
	if status.Healthy {
		t.Fatal("duplicate Codex registration unexpectedly healthy")
	}
	states := map[string]string{}
	for _, harness := range status.Harnesses {
		states[harness.Name] = harness.State
	}
	if states["codex"] != "duplicate" || states["omp"] != "installed" {
		t.Fatalf("unexpected harness states: %#v", states)
	}

	if err := os.RemoveAll(filepath.Join(home, ".codex", "skills", "agentctl-portable")); err != nil {
		t.Fatal(err)
	}
	status = buildBootstrapStatus(home, []string{"codex", "omp"}, func(string) string { return "" })
	if !status.Healthy {
		t.Fatalf("reconciled status unhealthy: %#v", status.Problems)
	}
	if status.SkillRevision != "tree:v0.1.4" || status.SkillDigest == "" {
		t.Fatalf("missing provenance: %#v", status)
	}
	writeBootstrapSkill(t, filepath.Join(home, ".agents", "skills"), "tree:v0.1.3", "portable")
	status = buildBootstrapStatus(home, []string{"codex", "omp"}, func(string) string { return "" })
	if status.Healthy || !containsString(status.Problems, "skill_too_old") {
		t.Fatalf("outdated portable skill unexpectedly healthy: %#v", status)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestBootstrapStatusDetectsCrossHarnessDriftAndMissing(t *testing.T) {
	originalVersion := version
	version = "v0.1.4"
	t.Cleanup(func() { version = originalVersion })
	home := t.TempDir()
	writeBootstrapSkill(t, filepath.Join(home, ".agents", "skills"), "tree:v0.1.4", "one")
	writeBootstrapSkill(t, filepath.Join(home, ".hermes", "skills"), "tree:v0.1.5", "two")
	writeExecutable(t, filepath.Join(home, ".local", "bin", "agentctl"))

	status := buildBootstrapStatus(home, []string{"codex", "hermes", "cursor"}, func(string) string { return "" })
	if status.Healthy {
		t.Fatal("drifted and missing status unexpectedly healthy")
	}
	want := map[string]bool{"cursor_missing": true, "skill_digest_drift": true, "skill_revision_drift": true}
	for _, problem := range status.Problems {
		delete(want, problem)
	}
	if len(want) != 0 {
		t.Fatalf("missing problems: %#v; got %#v", want, status.Problems)
	}
}

func TestPortableVersionComparison(t *testing.T) {
	for _, test := range []struct {
		current string
		want    bool
	}{{"v0.1.3", false}, {"v0.1.4", true}, {"0.2.0", true}, {"v0.1.2", false}, {"dev", false}} {
		if got := versionAtLeast(test.current, minimumPortableVersion); got != test.want {
			t.Fatalf("versionAtLeast(%q)=%v want %v", test.current, got, test.want)
		}
	}
}

func TestBootstrapDetectsExecutableHarnessesOnly(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := detectBootstrapHarnesses(home, func(key string) string {
		if key == "PATH" {
			return filepath.Join(home, "bin")
		}
		return ""
	}); len(got) != 0 {
		t.Fatalf("stale harness directory was detected: %v", got)
	}
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(home, "bin", "codex"))
	got := detectBootstrapHarnesses(home, func(key string) string {
		if key == "PATH" {
			return filepath.Join(home, "bin")
		}
		return ""
	})
	if len(got) != 1 || got[0] != "codex" {
		t.Fatalf("detected harnesses = %v", got)
	}
}

func TestBootstrapUpdateInstallsAndDeduplicatesSharedRoot(t *testing.T) {
	home := t.TempDir()
	for _, executable := range []string{"codex", "omp"} {
		bin := filepath.Join(home, "bin", executable)
		writeExecutable(t, bin)
	}
	var stdout bytes.Buffer
	a := &app{stdout: &stdout, stderr: &bytes.Buffer{}, getenv: func(key string) string {
		if key == "PATH" {
			return filepath.Join(home, "bin")
		}
		return ""
	}}
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	if problem := a.bootstrapUpdate(renderer, home, []string{"codex", "omp"}, "", false); problem != nil {
		t.Fatalf("bootstrap update failed: %v", problem)
	}
	var doc struct {
		Result bootstrapUpdateResult `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Result.Actions) != 1 || !doc.Result.Actions[0].Changed {
		t.Fatalf("expected one shared-root action: %#v", doc.Result)
	}
	root := filepath.Join(home, ".agents", "skills", "agentctl-portable")
	skill, err := portableasset.Skill()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
	if err != nil || !bytes.Equal(data, skill.Bytes) {
		t.Fatalf("installed embedded skill mismatch: %v", err)
	}
	stdout.Reset()
	if problem := a.bootstrapUpdate(renderer, home, []string{"codex", "omp"}, "", false); problem != nil {
		t.Fatalf("idempotent update failed: %v", problem)
	}
	if strings.Contains(stdout.String(), `"changed":true`) {
		t.Fatalf("idempotent update planned a write: %s", stdout.String())
	}
}

func TestBootstrapUpdateRefusesModifiedManagedSkill(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agents", "skills")
	writeBootstrapSkill(t, root, "tree:v0.1.6", "modified")
	var stdout bytes.Buffer
	a := &app{stdout: &stdout, stderr: &bytes.Buffer{}, getenv: func(string) string { return "" }}
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	problem := a.bootstrapUpdate(renderer, home, []string{"codex"}, "", false)
	if problem == nil || problem.Code != output.CodeConflict {
		t.Fatalf("modified managed skill was accepted: %#v", problem)
	}
	data, err := os.ReadFile(filepath.Join(root, "agentctl-portable", "SKILL.md"))
	if err != nil || string(data) != "modified" {
		t.Fatalf("conflicting skill changed: %q (%v)", data, err)
	}
}

func TestBootstrapUpdateRefusesManagedCompatibilityDuplicate(t *testing.T) {
	home := t.TempDir()
	compat := filepath.Join(home, ".codex", "skills")
	oldSkill := []byte("old portable skill")
	writeManagedFixture(t, compat, oldSkill, "tree:v0.1.6")
	var stdout bytes.Buffer
	a := &app{stdout: &stdout, stderr: &bytes.Buffer{}, getenv: func(string) string { return "" }}
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	if problem := a.bootstrapUpdate(renderer, home, []string{"codex"}, "", false); problem == nil || problem.Code != output.CodeConflict {
		t.Fatalf("managed compatibility duplicate was accepted: %#v", problem)
	}
	canonical := filepath.Join(home, ".agents", "skills", "agentctl-portable", "SKILL.md")
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("canonical skill was unexpectedly installed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(compat, "agentctl-portable", "SKILL.md"))
	if err != nil || !bytes.Equal(data, oldSkill) {
		t.Fatalf("compatibility copy changed: %q (%v)", data, err)
	}
}

func TestBootstrapUpdateRejectsPartialMulticaInstall(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "multica-skills")
	var stdout bytes.Buffer
	a := &app{stdout: &stdout, stderr: &bytes.Buffer{}, getenv: func(string) string { return "" }}
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	problem := a.bootstrapUpdate(renderer, home, []string{"multica"}, target, false)
	if problem == nil || problem.Code != output.CodeCapabilityUnavailable {
		t.Fatalf("partial Multica install was accepted: %#v", problem)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("partial Multica target was created: %v", err)
	}
}

func TestBootstrapUpdateUpgradesHashBoundLegacyManifest(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agents", "skills")
	oldSkill := []byte("old portable skill")
	writeManagedFixture(t, root, oldSkill, "tree:v0.1.6")
	var stdout bytes.Buffer
	a := &app{stdout: &stdout, stderr: &bytes.Buffer{}, getenv: func(string) string { return "" }}
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	if problem := a.bootstrapUpdate(renderer, home, []string{"codex"}, "", false); problem != nil {
		t.Fatalf("legacy upgrade failed: %v", problem)
	}
	skill, err := portableasset.Skill()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "agentctl-portable", "SKILL.md"))
	if err != nil || !bytes.Equal(data, skill.Bytes) {
		t.Fatalf("legacy skill was not upgraded: %v", err)
	}
}

func TestBootstrapUpdateRefusesInvalidManagedMarker(t *testing.T) {
	home := t.TempDir()
	var stdout bytes.Buffer
	a := &app{stdout: &stdout, stderr: &bytes.Buffer{}, getenv: func(string) string { return "" }}
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	if problem := a.bootstrapUpdate(renderer, home, []string{"codex"}, "", false); problem != nil {
		t.Fatal(problem)
	}
	markerPath := filepath.Join(home, ".agents", "skills", "agentctl-portable", bootstrapManagedMarkerName)
	invalid := []byte(`{"schema_version":1,"managed_by":"agentctl","skill_digest":"sha256:tampered"}`)
	if err := os.WriteFile(markerPath, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	problem := a.bootstrapUpdate(renderer, home, []string{"codex"}, "", false)
	if problem == nil || problem.Code != output.CodeConflict {
		t.Fatalf("invalid managed marker was adopted: %#v", problem)
	}
	data, err := os.ReadFile(markerPath)
	if err != nil || !bytes.Equal(data, invalid) {
		t.Fatalf("invalid marker changed: %q (%v)", data, err)
	}
}

func TestBootstrapUpdateRefusesInvalidManagedMarkerDuringUpgrade(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agents", "skills")
	writeManagedFixture(t, root, []byte("old portable skill"), "tree:v0.1.6")
	markerPath := filepath.Join(root, "agentctl-portable", bootstrapManagedMarkerName)
	invalid := []byte(`{"schema_version":1,"managed_by":"agentctl","skill_digest":"sha256:tampered"}`)
	if err := os.WriteFile(markerPath, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	a := &app{stdout: &stdout, stderr: &bytes.Buffer{}, getenv: func(string) string { return "" }}
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	problem := a.bootstrapUpdate(renderer, home, []string{"codex"}, "", false)
	if problem == nil || problem.Code != output.CodeConflict {
		t.Fatalf("invalid managed marker allowed upgrade: %#v", problem)
	}
	data, err := os.ReadFile(markerPath)
	if err != nil || !bytes.Equal(data, invalid) {
		t.Fatalf("invalid marker changed during upgrade: %q (%v)", data, err)
	}
}

func TestBootstrapStatusMarksSkillOlderThanEmbeddedRelease(t *testing.T) {
	originalVersion := version
	version = "v0.1.7"
	t.Cleanup(func() { version = originalVersion })
	home := t.TempDir()
	writeBootstrapSkill(t, filepath.Join(home, ".agents", "skills"), "tree:v0.1.6", "old portable skill")
	status := buildBootstrapStatus(home, []string{"codex"}, func(string) string { return "" })
	if status.Healthy || !containsString(status.Problems, "codex_outdated") {
		t.Fatalf("embedded-release drift was not reported: %#v", status)
	}
}

func TestBootstrapUpdateDoesNotInventMulticaRoot(t *testing.T) {
	home := t.TempDir()
	var stdout bytes.Buffer
	a := &app{stdout: &stdout, stderr: &bytes.Buffer{}, getenv: func(string) string { return "" }}
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	if problem := a.bootstrapUpdate(renderer, home, []string{}, "", false); problem != nil {
		t.Fatal(problem)
	}
	if _, err := os.Stat(filepath.Join(home, ".multica")); !os.IsNotExist(err) {
		t.Fatalf("bootstrap invented a Multica root: %v", err)
	}
}

func writeBootstrapSkill(t *testing.T, root, revision, body string) {
	t.Helper()
	dir := filepath.Join(root, "agentctl-portable")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"revision":"` + revision + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "revision-manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeManagedFixture(t *testing.T, root string, skill []byte, revision string) {
	t.Helper()
	dir := filepath.Join(root, "agentctl-portable")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), skill, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(skill)
	manifest := `{"schema_version":1,"manifest_kind":"agentctl_distribution_revision","distribution_id":"agentctl-portable","revision":"` + revision + `","assets":[{"id":"portable-skill","sha256":"` + hex.EncodeToString(sum[:]) + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, "revision-manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
}
