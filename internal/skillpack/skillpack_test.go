package skillpack

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, root string, manifest Manifest) Source {
	t.Helper()
	for _, skill := range manifest.Skills {
		directory := filepath.Join(root, filepath.FromSlash(skill.Path))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: "+skill.Name+"\n---\n\n# Test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, DefaultManifest), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return Source{RepositoryRoot: root, Remote: "git@example.test:david/agent-environment.git", Commit: "0123456789abcdef0123456789abcdef01234567", ManifestPath: DefaultManifest}
}

func TestReconcileInstallsSharedAndDistinctNativeRootsIdempotently(t *testing.T) {
	repository := t.TempDir()
	home := t.TempDir()
	source := writeFixture(t, repository, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"codex", "omp", "cursor"}}}})
	options := Options{Source: source, Home: home, DetectedHarnesses: []string{"codex", "omp", "cursor"}}

	plan, err := Plan(options)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Healthy || plan.Changed != 2 || len(plan.Actions) != 2 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	if plan.Actions[0].State != "install" || plan.Actions[1].State != "install" {
		t.Fatalf("expected install actions: %#v", plan.Actions)
	}

	result, err := Reconcile(options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Healthy || result.Changed != 0 || result.Applied != 2 {
		t.Fatalf("reconcile did not converge: %#v", result)
	}
	for _, path := range []string{filepath.Join(home, ".agents", "skills", "test-skill"), filepath.Join(home, ".cursor", "skills", "test-skill")} {
		if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
			t.Fatal(err)
		}
		value, err := loadMarker(filepath.Join(path, MarkerName))
		if err != nil {
			t.Fatal(err)
		}
		if value.SourceCommit != source.Commit || value.ContentSHA256 == "" {
			t.Fatalf("marker lacks provenance: %#v", value)
		}
	}
}

func TestEmptyPackIsHealthyAndReadOnly(t *testing.T) {
	repository := t.TempDir()
	home := t.TempDir()
	source := writeFixture(t, repository, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{}})
	plan, err := Plan(Options{Source: source, Home: home, DetectedHarnesses: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Healthy || len(plan.Actions) != 0 || plan.Changed != 0 || plan.Applied != 0 {
		t.Fatalf("unexpected empty-pack plan: %#v", plan)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("empty plan mutated home: %v", err)
	}
}

func TestDriftAndUnmanagedCollisionFailClosed(t *testing.T) {
	repository := t.TempDir()
	home := t.TempDir()
	source := writeFixture(t, repository, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"codex"}}}})
	options := Options{Source: source, Home: home, DetectedHarnesses: []string{"codex"}}
	if _, err := Reconcile(options); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(home, ".agents", "skills", "test-skill")
	if err := os.WriteFile(filepath.Join(destination, "SKILL.md"), []byte("local edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Plan(options)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Healthy || plan.Conflicts != 1 || plan.Actions[0].State != "drifted" {
		t.Fatalf("drift was not reported: %#v", plan)
	}
	if _, err := Reconcile(options); !errors.Is(err, ErrConflict) {
		t.Fatalf("reconcile should fail closed, got %v", err)
	}

	home2 := t.TempDir()
	unmanaged := filepath.Join(home2, ".agents", "skills", "test-skill")
	if err := os.MkdirAll(unmanaged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unmanaged, "SKILL.md"), []byte("unmanaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err = Plan(Options{Source: source, Home: home2, DetectedHarnesses: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Actions[0].State != "conflict" {
		t.Fatalf("unmanaged collision was not reported: %#v", plan)
	}
}

func TestMulticaIsExplicitlyUnsupportedAndUndetectedNativeIsSkipped(t *testing.T) {
	repository := t.TempDir()
	home := t.TempDir()
	source := writeFixture(t, repository, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"cursor", "multica"}}}})
	plan, err := Plan(Options{Source: source, Home: home, DetectedHarnesses: nil})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Healthy || plan.Unsupported != 1 || len(plan.Actions) != 2 {
		t.Fatalf("unexpected authority plan: %#v", plan)
	}
	states := map[string]bool{}
	for _, action := range plan.Actions {
		states[action.State] = true
	}
	if !states["unsupported"] || !states["not_detected"] {
		t.Fatalf("authority states missing: %#v", plan.Actions)
	}
}

func TestConcurrentReconcileFailsClosed(t *testing.T) {
	repository := t.TempDir()
	home := t.TempDir()
	source := writeFixture(t, repository, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"codex"}}}})
	lock, err := acquireLock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	_, err = Reconcile(Options{Source: source, Home: home, DetectedHarnesses: []string{"codex"}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent reconcile should conflict, got %v", err)
	}
}

func TestRejectsSymlinkedSkillContent(t *testing.T) {
	repository := t.TempDir()
	home := t.TempDir()
	directory := filepath.Join(repository, "skills", "test-skill")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("# Test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("SKILL.md", filepath.Join(directory, "alias.md")); err != nil {
		t.Fatal(err)
	}
	source := writeFixture(t, repository, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"codex"}}}})
	if _, err := Plan(Options{Source: source, Home: home, DetectedHarnesses: []string{"codex"}}); err == nil {
		t.Fatal("symlinked content was accepted")
	}
}
