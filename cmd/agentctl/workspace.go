package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

var errNotGitWorkspace = errors.New("not a Git worktree")

type workspaceOwner struct {
	ExecutionID ids.ExecutionID         `json:"execution_id"`
	Adapter     string                  `json:"adapter"`
	State       model.State             `json:"state"`
	Liveness    model.Liveness          `json:"liveness"`
	CreatedAt   time.Time               `json:"created_at"`
	StartedAt   *time.Time              `json:"started_at,omitempty"`
	UpdatedAt   time.Time               `json:"updated_at"`
	Workspace   model.WorkspaceIdentity `json:"workspace"`
}

type workspaceTarget struct {
	RequestedPath string                  `json:"requested_path"`
	Workspace     model.WorkspaceIdentity `json:"workspace"`
}

type workspaceOwnersDocument struct {
	SchemaVersion                int              `json:"schema_version"`
	Authority                    string           `json:"authority"`
	OwnershipSemantics           string           `json:"ownership_semantics"`
	Exclusive                    bool             `json:"exclusive"`
	HostLocal                    bool             `json:"host_local"`
	Target                       *workspaceTarget `json:"target,omitempty"`
	Owners                       []workspaceOwner `json:"owners"`
	UnattributedNonterminalCount int              `json:"unattributed_nonterminal_count"`
	EvidenceComplete             bool             `json:"evidence_complete"`
}

func (a *app) workspaceCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) == 0 || args[0] != "owners" {
		return output.NewError(output.CodeUsage, "usage: agentctl workspace owners [--path directory]", false)
	}
	path, problem := parseWorkspaceOwners(args[1:])
	if problem != nil {
		return problem
	}
	var target *workspaceTarget
	if path != "" {
		resolved, err := resolveWorkspacePath(path)
		if err != nil {
			return output.Wrap(output.CodeUsage, "resolve workspace path", false, err).WithDetail("path", path)
		}
		workspace, err := discoverGitWorkspace(ctx, resolved)
		if errors.Is(err, errNotGitWorkspace) {
			return output.NewError(output.CodeCapabilityUnavailable, "path is not inside a discoverable Git worktree", false).
				WithDetail("path", resolved).
				WithDetail("diagnostic_code", "workspace_not_git")
		}
		if err != nil {
			return output.Wrap(output.CodeCapabilityUnavailable, "inspect Git worktree", true, err).
				WithDetail("path", resolved).
				WithDetail("diagnostic_code", "workspace_inspection_failed")
		}
		target = &workspaceTarget{RequestedPath: resolved, Workspace: *workspace}
	}

	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	executions, err := journal.ListExecutions(ctx, true)
	if err != nil {
		return mapStoreError("list active workspace executions", err)
	}
	owners := make([]workspaceOwner, 0, len(executions))
	unattributed := 0
	for _, execution := range executions {
		if execution.Workspace == nil {
			// Old journal rows remain readable. Their missing attribution is
			// surfaced instead of being treated as evidence that a worktree is
			// unused.
			unattributed++
			continue
		}
		if target != nil && !sameGitWorktree(*execution.Workspace, target.Workspace) {
			continue
		}
		owners = append(owners, workspaceOwner{
			ExecutionID: execution.ID,
			Adapter:     execution.Adapter,
			State:       execution.State,
			Liveness:    execution.Liveness,
			CreatedAt:   execution.CreatedAt,
			StartedAt:   execution.StartedAt,
			UpdatedAt:   execution.UpdatedAt,
			Workspace:   *execution.Workspace,
		})
	}
	sort.Slice(owners, func(i, j int) bool {
		if owners[i].CreatedAt.Equal(owners[j].CreatedAt) {
			return owners[i].ExecutionID.String() < owners[j].ExecutionID.String()
		}
		return owners[i].CreatedAt.Before(owners[j].CreatedAt)
	})
	document := workspaceOwnersDocument{
		SchemaVersion:                1,
		Authority:                    "agentctl_execution_journal",
		OwnershipSemantics:           "nonterminal_execution_launch_provenance",
		Exclusive:                    false,
		HostLocal:                    true,
		Target:                       target,
		Owners:                       owners,
		UnattributedNonterminalCount: unattributed,
		EvidenceComplete:             unattributed == 0,
	}
	lines := make([]output.Line, 0, len(owners)+1)
	for _, owner := range owners {
		lines = append(lines, output.Line{Lead: owner.ExecutionID.String(), Fields: []output.Field{
			{Name: "state", Value: owner.State},
			{Name: "liveness", Value: owner.Liveness},
			{Name: "worktree", Value: owner.Workspace.Root},
			{Name: "exclusive", Value: false},
		}})
	}
	if unattributed != 0 {
		lines = append(lines, output.Line{Lead: "coverage", Fields: []output.Field{{Name: "unattributed_nonterminal", Value: unattributed}, {Name: "complete", Value: false}}})
	}
	if err := renderer.Success(output.Success{Result: document, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write workspace owners", false, err)
	}
	return nil
}

func parseWorkspaceOwners(args []string) (string, *output.Error) {
	path := ""
	for i := 0; i < len(args); i++ {
		if args[i] != "--path" {
			return "", output.NewError(output.CodeUsage, "unknown workspace owners flag", false).WithDetail("flag", args[i])
		}
		if path != "" {
			return "", output.NewError(output.CodeUsage, "--path may be supplied only once", false)
		}
		if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
			return "", output.NewError(output.CodeUsage, "--path requires a directory", false)
		}
		i++
		path = args[i]
	}
	return path, nil
}

func resolveWorkspacePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		absolute = filepath.Dir(absolute)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return absolute, nil
}

func captureLaunchWorkspace(ctx context.Context, rawCWD string) (*string, *string, *model.WorkspaceIdentity, error) {
	cwd := rawCWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	resolved, err := filepath.Abs(filepath.Clean(cwd))
	if err != nil {
		return nil, nil, nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, nil, errors.New("working directory is not a directory")
	}
	if evaluated, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
		resolved = evaluated
	}
	cwdCopy := resolved
	workspace, err := discoverGitWorkspace(ctx, resolved)
	if errors.Is(err, errNotGitWorkspace) {
		return &cwdCopy, nil, nil, nil
	}
	if err != nil {
		// Workspace provenance is supporting evidence. A Git metadata read must
		// never prevent the explicitly requested native launch.
		return &cwdCopy, nil, nil, nil
	}
	repository := workspace.Root
	return &cwdCopy, &repository, workspace, nil
}

func discoverGitWorkspace(ctx context.Context, cwd string) (*model.WorkspaceIdentity, error) {
	root, err := gitRead(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, errNotGitWorkspace
	}
	gitDir, err := gitRead(ctx, cwd, "rev-parse", "--git-dir")
	if err != nil {
		return nil, fmt.Errorf("git dir: %w", err)
	}
	commonDir, err := gitRead(ctx, cwd, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("git common dir: %w", err)
	}
	root, err = canonicalGitPath(cwd, root)
	if err != nil {
		return nil, fmt.Errorf("repository root: %w", err)
	}
	gitDir, err = canonicalGitPath(cwd, gitDir)
	if err != nil {
		return nil, fmt.Errorf("git dir path: %w", err)
	}
	commonDir, err = canonicalGitPath(cwd, commonDir)
	if err != nil {
		return nil, fmt.Errorf("git common dir path: %w", err)
	}
	headOID, _ := gitRead(ctx, cwd, "rev-parse", "--verify", "HEAD")
	headRef, _ := gitRead(ctx, cwd, "symbolic-ref", "-q", "HEAD")
	operations, err := gitOperationRefs(gitDir)
	if err != nil {
		return nil, err
	}
	return &model.WorkspaceIdentity{
		Kind:          model.WorkspaceGitWorktree,
		Root:          root,
		GitDir:        gitDir,
		GitCommonDir:  commonDir,
		HeadOID:       strings.TrimSpace(headOID),
		HeadRef:       strings.TrimSpace(headRef),
		OperationRefs: operations,
	}, nil
}

func gitRead(ctx context.Context, cwd string, args ...string) (string, error) {
	argv := append([]string{"-C", cwd, "--no-optional-locks"}, args...)
	command := exec.CommandContext(ctx, "git", argv...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	if len(output) > 64<<10 {
		return "", errors.New("git output exceeds 64 KiB")
	}
	return strings.TrimSpace(string(output)), nil
}

func canonicalGitPath(cwd, value string) (string, error) {
	if !filepath.IsAbs(value) {
		value = filepath.Join(cwd, value)
	}
	value, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(value); resolveErr == nil {
		value = resolved
	}
	return value, nil
}

func gitOperationRefs(gitDir string) ([]string, error) {
	markers := []struct {
		name  string
		paths []string
	}{
		{name: "bisect", paths: []string{"BISECT_LOG", "BISECT_START"}},
		{name: "cherry_pick", paths: []string{"CHERRY_PICK_HEAD"}},
		{name: "merge", paths: []string{"MERGE_HEAD"}},
		{name: "rebase", paths: []string{"rebase-apply", "rebase-merge"}},
		{name: "revert", paths: []string{"REVERT_HEAD"}},
	}
	operations := []string{}
	for _, marker := range markers {
		found := false
		for _, relative := range marker.paths {
			_, err := os.Stat(filepath.Join(gitDir, relative))
			switch {
			case err == nil:
				found = true
			case os.IsNotExist(err):
			default:
				return nil, fmt.Errorf("inspect %s operation marker: %w", marker.name, err)
			}
		}
		if found {
			operations = append(operations, marker.name)
		}
	}
	return operations, nil
}

func sameGitWorktree(a, b model.WorkspaceIdentity) bool {
	return a.Kind == model.WorkspaceGitWorktree && b.Kind == model.WorkspaceGitWorktree && a.GitDir == b.GitDir
}
