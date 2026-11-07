package skillpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The operator's Git rules can rewrite a configured SCP-style Skill Hub remote
// to another transport. The managed checkout must still count as in sync.
func TestHubStatusAcceptsInsteadOfRewrittenRemote(t *testing.T) {
	parent := t.TempDir()
	hub := filepath.Join(parent, "hub")
	if err := os.Mkdir(hub, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, hub, "init", "--quiet", "-b", "main")
	writeFixture(t, hub, Manifest{SchemaVersion: SchemaVersion, Skills: []Skill{{Name: "test-skill", Path: "skills/test-skill", Targets: []string{"codex"}}}})
	fixtureGit(t, hub, "add", ".")
	fixtureGit(t, hub, "-c", "user.name=Agentctl Test", "-c", "user.email=agentctl@example.test", "commit", "--quiet", "-m", "initial")
	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[url \""+parent+"/\"]\n\tinsteadOf = git@agentctl-test.invalid:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)

	home := t.TempDir()
	selection := HubSelection{Remote: "git@agentctl-test.invalid:hub", Ref: "main", ManifestPath: DefaultManifest}
	first, err := UpdateHubSource(context.Background(), home, selection, time.Now())
	if err != nil || !first.Status.InSync {
		t.Fatalf("initialize: %#v %v", first, err)
	}
	status, err := HubSourceStatusReadOnly(context.Background(), home, selection)
	if err != nil || !status.InSync {
		t.Fatalf("status through insteadOf remote: %#v %v", status, err)
	}
}

// Git fetches from the first stored origin URL; a second URL must show as drift.
func TestHubStatusReportsExtraOriginURL(t *testing.T) {
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
	fixtureGit(t, checkout, "config", "--replace-all", "remote.origin.url", filepath.Join(t.TempDir(), "other.git"))
	fixtureGit(t, checkout, "config", "--add", "remote.origin.url", hub)
	status, err := HubSourceStatusReadOnly(context.Background(), home, selection)
	if err != nil || status.InSync {
		t.Fatalf("extra origin URL not reported: %#v %v", status, err)
	}
	// An empty extra value is still a second URL.
	fixtureGit(t, checkout, "config", "--replace-all", "remote.origin.url", hub)
	fixtureGit(t, checkout, "config", "--add", "remote.origin.url", "")
	status, err = HubSourceStatusReadOnly(context.Background(), home, selection)
	if err != nil || status.InSync {
		t.Fatalf("empty extra origin URL not reported: %#v %v", status, err)
	}
}
