package context

import (
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/knowledge"
)

func TestSelectContextDeterministic(t *testing.T) {
	b := knowledge.Bundle{Manifest: knowledge.Manifest{BundleRevision: "sha256:bundle", Sources: []knowledge.SourceRevision{{ID: "repo-amber-willow-orbit-tiger-harbor-gentle", RemoteURL: "ssh://git@example.test/team/repo.git", Commit: "deadbeef"}}}, Records: []knowledge.Record{{ID: "knowledge-a", Title: "Runbook", Text: "restart safely", Mode: knowledge.ModeLoose, SourceRepoID: "repo-amber-willow-orbit-tiger-harbor-gentle", Scope: map[string][]string{"task_kinds": {"reliability"}}, Provenance: knowledge.Provenance{Path: "runbook.md", ContentDigest: "sha256:d"}}, {ID: "knowledge-expired", Text: "old", ExpiresAt: "2020-01-01", SourceRepoID: "repo-amber-willow-orbit-tiger-harbor-gentle"}}}
	s := SelectContext(b, SelectionInput{TaskKind: "reliability", Now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if len(s.Matches) != 1 || s.Matches[0].Record.ID != "knowledge-a" {
		t.Fatalf("unexpected selection: %#v", s)
	}
	if got := Render(s, 1000); got == "" {
		t.Fatal("empty render")
	}
}
