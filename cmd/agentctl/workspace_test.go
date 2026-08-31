package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestDiscoverGitWorkspaceDistinguishesLinkedWorktreesAndOperations(t *testing.T) {
	repository := initWorkspaceRepository(t)
	siblingRoot := t.TempDir()
	sibling := filepath.Join(siblingRoot, "sibling")
	runGit(t, repository, "worktree", "add", "-b", "sibling", sibling)

	rootWorkspace, err := discoverGitWorkspace(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	siblingWorkspace, err := discoverGitWorkspace(context.Background(), sibling)
	if err != nil {
		t.Fatal(err)
	}
	if rootWorkspace.GitCommonDir != siblingWorkspace.GitCommonDir {
		t.Fatalf("linked worktrees have different common dirs: root=%s sibling=%s", rootWorkspace.GitCommonDir, siblingWorkspace.GitCommonDir)
	}
	if rootWorkspace.GitDir == siblingWorkspace.GitDir {
		t.Fatalf("linked worktrees share git dir: %s", rootWorkspace.GitDir)
	}
	if rootWorkspace.HeadOID == "" || siblingWorkspace.HeadRef != "refs/heads/sibling" {
		t.Fatalf("missing HEAD provenance: root=%+v sibling=%+v", rootWorkspace, siblingWorkspace)
	}
	if err := os.Mkdir(filepath.Join(siblingWorkspace.GitDir, "rebase-merge"), 0o700); err != nil {
		t.Fatal(err)
	}
	duringRebase, err := discoverGitWorkspace(context.Background(), sibling)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(duringRebase.OperationRefs, []string{"rebase"}) {
		t.Fatalf("operation refs=%v", duringRebase.OperationRefs)
	}
}

func TestDirectRunCapturesWorkspaceProvenance(t *testing.T) {
	repository := initWorkspaceRepository(t)
	journalRoot := t.TempDir()
	if err := os.Chmod(journalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(journalRoot, "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--cwd", repository, "--adapter", "generic-process", "--allow-missing-result", "--", "/bin/echo", "done"})
	if code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var document struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Result.Workspace != nil {
		t.Fatalf("ordinary run output leaked local workspace paths: %s", stdout.String())
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := journal.GetExecution(context.Background(), document.Result.ID)
	_ = journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Workspace == nil || stored.CWD == nil || *stored.CWD != repository || stored.Repository == nil || *stored.Repository != repository {
		t.Fatalf("cwd/repository provenance mismatch: %+v", stored)
	}
}

func TestWorkspaceOwnersIsNonExclusiveAndLegacyRowsAreVisible(t *testing.T) {
	repository := initWorkspaceRepository(t)
	workspace, err := discoverGitWorkspace(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	journalRoot := t.TempDir()
	if err := os.Chmod(journalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(journalRoot, "journal.db")
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	active, _, err := journal.CreateExecution(context.Background(), workspaceTestExecution(now, workspace), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := journal.CreateExecution(context.Background(), workspaceTestExecution(now.Add(time.Second), nil), contracts.MutationKey{}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journalPath, "workspace", "owners", "--path", repository}); code != 0 {
		t.Fatalf("owners exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var document struct {
		Result workspaceOwnersDocument `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	result := document.Result
	if result.Exclusive || result.OwnershipSemantics != "nonterminal_execution_launch_provenance" {
		t.Fatalf("ownership semantics overclaim: %+v", result)
	}
	if len(result.Owners) != 1 || result.Owners[0].ExecutionID != active.ID {
		t.Fatalf("owners=%+v want=%s", result.Owners, active.ID)
	}
	if result.UnattributedNonterminalCount != 1 || result.EvidenceComplete {
		t.Fatalf("legacy coverage was hidden: %+v", result)
	}
}

func workspaceTestExecution(now time.Time, workspace *model.WorkspaceIdentity) model.Execution {
	return model.Execution{
		Authority:      model.AuthorityNative,
		Adapter:        "generic-process",
		Mode:           model.ModeDirect,
		Acquisition:    model.AcquisitionLaunched,
		State:          model.StateRunning,
		Liveness:       model.LivenessAlive,
		SourceBindings: []model.SourceBinding{},
		Capabilities: model.CapabilitySnapshot{
			NegotiatedAt:   now,
			AdapterVersion: "test",
			Items:          []model.CapabilityItem{},
		},
		Workspace: workspace,
		Observation: model.Observation{
			Source:     model.ObservationNativeStream,
			Integrity:  model.IntegrityVerified,
			ObservedAt: now,
		},
	}
}

func initWorkspaceRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Agentctl Test")
	runGit(t, repository, "config", "user.email", "agentctl@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "-m", "fixture")
	resolved, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func runGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, output)
	}
}
