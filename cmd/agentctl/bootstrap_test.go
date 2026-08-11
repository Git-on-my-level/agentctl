package main

import (
	"os"
	"path/filepath"
	"testing"
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

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
}
