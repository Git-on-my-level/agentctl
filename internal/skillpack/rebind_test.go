package skillpack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHubRebindProvesAncestryBeforeChangingRemote(t *testing.T) {
	hub := t.TempDir()
	fixtureGit(t, hub, "init", "--quiet", "-b", "main")
	writeFixture(t, hub, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"codex"}}}})
	fixtureGit(t, hub, "add", ".")
	fixtureGit(t, hub, "-c", "user.name=Agentctl Test", "-c", "user.email=agentctl@example.test", "commit", "--quiet", "-m", "initial")
	home := t.TempDir()
	selection := HubSelection{Remote: hub, Ref: "main", ManifestPath: DefaultManifest}
	first, err := UpdateHubSource(context.Background(), home, selection, time.Now())
	if err != nil || !first.Status.InSync {
		t.Fatalf("initialize: %#v %v", first, err)
	}
	checkout := first.Status.CheckoutPath
	origin := fixtureGit(t, checkout, "remote", "get-url", "origin")
	if origin != hub {
		t.Fatalf("origin=%s", origin)
	}
	same, err := PlanHubRebind(context.Background(), home, selection)
	if err != nil || same.Applicable || same.Reason != "already_bound" || same.NetworkAccess {
		t.Fatalf("already bound: %#v %v", same, err)
	}
	stateBefore, err := os.ReadFile(first.Status.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanHubRebind(context.Background(), home, selection); err != nil {
		t.Fatal(err)
	}
	stateAfter, err := os.ReadFile(first.Status.StatePath)
	if err != nil || string(stateAfter) != string(stateBefore) || fixtureGit(t, checkout, "remote", "get-url", "origin") != origin {
		t.Fatal("plan rewrote Skill Hub state or origin")
	}

	mirror := filepath.Join(t.TempDir(), "mirror.git")
	fixtureGit(t, hub, "clone", "--bare", hub, mirror)
	next := HubSelection{Remote: mirror, Ref: "main", ManifestPath: DefaultManifest}
	if err := os.WriteFile(filepath.Join(checkout, "local.txt"), []byte("drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty, err := PlanHubRebind(context.Background(), home, next)
	if err != nil || dirty.Applicable || dirty.Reason != "checkout_dirty" {
		t.Fatalf("dirty=%#v %v", dirty, err)
	}
	if err := os.Remove(filepath.Join(checkout, "local.txt")); err != nil {
		t.Fatal(err)
	}
	refChange, err := PlanHubRebind(context.Background(), home, HubSelection{Remote: hub, Ref: "other", ManifestPath: DefaultManifest})
	if err != nil || refChange.Applicable || refChange.Reason != "ref_changed" {
		t.Fatalf("ref=%#v %v", refChange, err)
	}
	plan, err := PlanHubRebind(context.Background(), home, next)
	if err != nil || !plan.Applicable || plan.Reason != "remote_changed" || !strings.HasPrefix(plan.Digest, "sha256:") || plan.NetworkAccess || plan.AppliedCommit != first.Status.AppliedCommit {
		t.Fatalf("plan=%#v %v", plan, err)
	}
	if fixtureGit(t, checkout, "remote", "get-url", "origin") != origin {
		t.Fatal("plan fetched or changed origin")
	}
	if _, err := ApplyHubRebind(context.Background(), home, next, "sha256:"+strings.Repeat("ab", 32), time.Now()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("wrong digest: %v", err)
	}
	if fixtureGit(t, checkout, "remote", "get-url", "origin") != origin {
		t.Fatal("digest mismatch changed origin")
	}
	applied, err := ApplyHubRebind(context.Background(), home, next, plan.Digest, time.Now())
	if err != nil || !applied.Applied || !applied.NetworkAccess || applied.AppliedCommit != plan.AppliedCommit {
		t.Fatalf("apply=%#v %v", applied, err)
	}
	if got := fixtureGit(t, checkout, "remote", "get-url", "origin"); got != mirror {
		t.Fatalf("origin=%s", got)
	}
	retry, err := ApplyHubRebind(context.Background(), home, next, plan.Digest, time.Now())
	if err != nil || retry.Applied || retry.NetworkAccess || retry.Reason != "already_bound" {
		t.Fatalf("applied digest retry=%#v %v", retry, err)
	}
	if got := fixtureGit(t, checkout, "remote", "get-url", "origin"); got != mirror {
		t.Fatalf("retry changed origin to %s", got)
	}
	if _, err := ApplyHubRebind(context.Background(), home, next, "sha256:"+strings.Repeat("cd", 32), time.Now()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("stale digest after apply: %v", err)
	}
	if head := fixtureGit(t, checkout, "rev-parse", "HEAD"); head != plan.AppliedCommit {
		t.Fatalf("checkout moved from %s to %s", plan.AppliedCommit, head)
	}
	unrelated := t.TempDir()
	fixtureGit(t, unrelated, "init", "--quiet", "-b", "main")
	writeFixture(t, unrelated, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"codex"}}}})
	fixtureGit(t, unrelated, "add", ".")
	fixtureGit(t, unrelated, "-c", "user.name=Agentctl Test", "-c", "user.email=agentctl@example.test", "commit", "--quiet", "-m", "other")
	foreign := HubSelection{Remote: unrelated, Ref: "main", ManifestPath: DefaultManifest}
	foreignPlan, err := PlanHubRebind(context.Background(), home, foreign)
	if err != nil || !foreignPlan.Applicable {
		t.Fatalf("foreign plan=%#v %v", foreignPlan, err)
	}
	if _, err := ApplyHubRebind(context.Background(), home, foreign, foreignPlan.Digest, time.Now()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "pinned commit") {
		t.Fatalf("unrelated remote: %v", err)
	}
	if fixtureGit(t, checkout, "remote", "get-url", "origin") != mirror {
		t.Fatal("rejected rebind changed origin")
	}
	if _, err := UpdateHubSource(context.Background(), home, foreign, time.Now()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "remote_changed") {
		t.Fatalf("update conflict: %v", err)
	}
}
