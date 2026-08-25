package skillpack

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
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

func fixtureGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

type noHooksGit struct{}

func (noHooksGit) Run(ctx context.Context, directory string, args ...string) ([]byte, error) {
	return (config.NativeSourceGit{Binary: "/usr/bin/git"}).Run(ctx, directory, args...)
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

func TestAutoCleanPreservesDriftAndUpgradesOtherSkills(t *testing.T) {
	repository := t.TempDir()
	home := t.TempDir()
	manifest := Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{
		{Name: "drifted-skill", Path: "skills/drifted-skill", Targets: []string{"codex"}},
		{Name: "clean-skill", Path: "skills/clean-skill", Targets: []string{"codex"}},
	}}
	source := writeFixture(t, repository, manifest)
	options := Options{Source: source, Home: home, DetectedHarnesses: []string{"codex"}}
	if _, err := Reconcile(options); err != nil {
		t.Fatal(err)
	}
	drifted := filepath.Join(home, ".agents", "skills", "drifted-skill", "SKILL.md")
	if err := os.WriteFile(drifted, []byte("local improvement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanSource := filepath.Join(repository, "skills", "clean-skill", "SKILL.md")
	if err := os.WriteFile(cleanSource, []byte("---\nname: clean-skill\n---\n\n# Updated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options.Source.Commit = "1123456789abcdef0123456789abcdef01234567"
	result, err := ReconcileAutoClean(options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Healthy || result.Conflicts != 1 || result.Applied != 1 {
		t.Fatalf("unexpected auto-clean result: %#v", result)
	}
	if data, _ := os.ReadFile(drifted); string(data) != "local improvement\n" {
		t.Fatalf("drifted skill was overwritten: %q", data)
	}
	cleanInstalled := filepath.Join(home, ".agents", "skills", "clean-skill", "SKILL.md")
	if data, _ := os.ReadFile(cleanInstalled); !strings.Contains(string(data), "# Updated") {
		t.Fatalf("clean skill was not upgraded: %q", data)
	}
}

func TestReservedBootstrapSkillIsRejected(t *testing.T) {
	repository := t.TempDir()
	home := t.TempDir()
	source := writeFixture(t, repository, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "agentctl-portable", Path: "skills/agentctl-portable", Targets: []string{"codex"}}}})
	if _, err := Plan(Options{Source: source, Home: home, DetectedHarnesses: []string{"codex"}}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved bootstrap skill was accepted: %v", err)
	}
}

func TestHubSourceInitializesAndAdvancesOnlyFastForwardCleanCheckout(t *testing.T) {
	hub := t.TempDir()
	fixtureGit(t, hub, "init", "--quiet", "-b", "main")
	writeFixture(t, hub, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"codex"}}}})
	manifest := filepath.Join(hub, DefaultManifest)
	if err := os.Rename(manifest, filepath.Join(hub, "fleet-core.json")); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(hub, "fleet-core.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hub, "alternate.json"), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, hub, "add", ".")
	fixtureGit(t, hub, "-c", "user.name=Agentctl Test", "-c", "user.email=agentctl@example.test", "commit", "--quiet", "-m", "initial")
	home := t.TempDir()
	selection := HubSelection{Remote: hub, Ref: "main", ManifestPath: "fleet-core.json"}
	first, err := UpdateHubSource(context.Background(), home, selection, time.Now())
	if err != nil || !first.Changed || !first.Status.InSync {
		t.Fatalf("initialize failed: %#v %v", first, err)
	}
	alternate := HubSelection{Remote: hub, Ref: "main", ManifestPath: "alternate.json"}
	selectionChange, err := UpdateHubSource(context.Background(), home, alternate, time.Now())
	if err != nil || !selectionChange.Changed || !selectionChange.Status.InSync || selectionChange.Status.Selection.ManifestPath != "alternate.json" {
		t.Fatalf("manifest selection change failed: %#v %v", selectionChange, err)
	}
	selection = alternate
	if err := os.WriteFile(filepath.Join(hub, "skills", "test-skill", "SKILL.md"), []byte("---\nname: test-skill\n---\n\n# New\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, hub, "add", ".")
	fixtureGit(t, hub, "-c", "user.name=Agentctl Test", "-c", "user.email=agentctl@example.test", "commit", "--quiet", "-m", "advance")
	second, err := UpdateHubSource(context.Background(), home, selection, time.Now())
	if err != nil || !second.Changed || second.Status.AppliedCommit == first.Status.AppliedCommit {
		t.Fatalf("advance failed: %#v %v", second, err)
	}
	if err := os.WriteFile(filepath.Join(second.Status.CheckoutPath, "local.txt"), []byte("drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateHubSource(context.Background(), home, selection, time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("dirty checkout was not rejected: %v", err)
	}
}

func TestDriftReviewRestoreAndProposalArePlanFirst(t *testing.T) {
	repository := t.TempDir()
	fixtureGit(t, repository, "init", "--quiet", "-b", "main")
	source := writeFixture(t, repository, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"codex"}}}})
	fixtureGit(t, repository, "add", ".")
	fixtureGit(t, repository, "-c", "user.name=Agentctl Test", "-c", "user.email=agentctl@example.test", "commit", "--quiet", "-m", "initial")
	source.Commit = fixtureGit(t, repository, "rev-parse", "HEAD")
	home := t.TempDir()
	options := Options{Source: source, Home: home, DetectedHarnesses: []string{"codex"}}
	if _, err := Reconcile(options); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(home, ".agents", "skills", "test-skill", "SKILL.md")
	if err := os.WriteFile(destination, []byte("local improvement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	review, err := ReviewDrift(options, "test-skill", "codex")
	if err != nil || len(review.Files) != 1 || review.Files[0].State != "modified" {
		t.Fatalf("unexpected drift review: %#v %v", review, err)
	}
	planned, err := RestoreDrift(options, "test-skill", "codex", false, time.Now())
	if err != nil || planned.Applied || planned.BackupPath != "" {
		t.Fatalf("restore plan mutated: %#v %v", planned, err)
	}
	if data, _ := os.ReadFile(destination); string(data) != "local improvement\n" {
		t.Fatal("restore plan changed the local skill")
	}
	restored, err := RestoreDrift(options, "test-skill", "codex", true, time.Now())
	if err != nil || !restored.Applied || restored.BackupPath == "" {
		t.Fatalf("restore failed: %#v %v", restored, err)
	}
	if data, _ := os.ReadFile(filepath.Join(restored.BackupPath, "SKILL.md")); string(data) != "local improvement\n" {
		t.Fatalf("backup omitted local content: %q", data)
	}
	if err := os.WriteFile(destination, []byte("publish this\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proposalPlan, err := ProposeDrift(context.Background(), noHooksGit{}, options, "test-skill", "codex", false, time.Now())
	if err != nil || proposalPlan.Applied || proposalPlan.WorktreePath != "" {
		t.Fatalf("proposal plan mutated: %#v %v", proposalPlan, err)
	}
	proposal, err := ProposeDrift(context.Background(), noHooksGit{}, options, "test-skill", "codex", true, time.Now())
	if err != nil || !proposal.Applied || proposal.WorktreePath == "" || proposal.Branch == "" {
		t.Fatalf("proposal failed: %#v %v", proposal, err)
	}
	if data, _ := os.ReadFile(filepath.Join(proposal.WorktreePath, "skills", "test-skill", "SKILL.md")); string(data) != "publish this\n" {
		t.Fatalf("proposal omitted local content: %q", data)
	}
	if _, err := os.Stat(filepath.Join(proposal.WorktreePath, "skills", "test-skill", MarkerName)); !os.IsNotExist(err) {
		t.Fatalf("proposal copied managed marker: %v", err)
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
