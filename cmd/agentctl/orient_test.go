package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

type fakeOrientGitResult struct {
	value string
	err   error
}

type fakeOrientGit map[string]fakeOrientGitResult

func (f fakeOrientGit) Run(_ context.Context, _ string, args ...string) (string, error) {
	result, ok := f[strings.Join(args, "\x00")]
	if !ok {
		return "", errors.New("unexpected git invocation: " + strings.Join(args, " "))
	}
	return result.value, result.err
}

func orientGitKey(args ...string) string { return strings.Join(args, "\x00") }

func TestInspectOrientWorkspaceReportsWorktreeWithoutFetching(t *testing.T) {
	invocations := fakeOrientGit{
		orientGitKey("rev-parse", "--is-inside-work-tree"):                               {value: "true"},
		orientGitKey("rev-parse", "--show-toplevel"):                                     {value: "/work/tree"},
		orientGitKey("rev-parse", "--absolute-git-dir"):                                  {value: "/work/repo/.git/worktrees/tree"},
		orientGitKey("rev-parse", "--git-common-dir"):                                    {value: "/work/repo/.git"},
		orientGitKey("rev-parse", "HEAD"):                                                {value: strings.Repeat("a", 40)},
		orientGitKey("symbolic-ref", "--quiet", "--short", "HEAD"):                       {value: "feature/orient"},
		orientGitKey("status", "--porcelain=v1", "--untracked-files=normal"):             {value: " M README.md"},
		orientGitKey("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"): {value: "origin/feature/orient"},
		orientGitKey("rev-list", "--left-right", "--count", "HEAD...@{upstream}"):        {value: "2\t3"},
	}
	deps := orientDependencies{getwd: func() (string, error) { return "/work/tree/subdir", nil }, git: invocations}
	got := inspectOrientWorkspace(context.Background(), deps)
	if got.Status != "ready" || got.WorktreeRoot != "/work/tree" || got.RepositoryName != "tree" {
		t.Fatalf("workspace=%+v", got)
	}
	if !got.LinkedWorktree || got.Branch != "feature/orient" || got.Detached || got.DirtyStatus != "dirty" {
		t.Fatalf("git projection=%+v", got)
	}
	if got.Upstream.Status != "ready" || got.Upstream.Ahead != 2 || got.Upstream.Behind != 3 {
		t.Fatalf("upstream=%+v", got.Upstream)
	}
	for invocation := range invocations {
		if strings.Contains(invocation, "fetch") {
			t.Fatalf("test fixture unexpectedly permits fetch: %q", invocation)
		}
	}
}

func TestInspectOrientWorkspaceDistinguishesUnknownAndNotRepository(t *testing.T) {
	t.Run("git unavailable", func(t *testing.T) {
		deps := orientDependencies{getwd: func() (string, error) { return "/work", nil }, git: fakeOrientGit{orientGitKey("rev-parse", "--is-inside-work-tree"): {err: errOrientGitUnavailable}}}
		got := inspectOrientWorkspace(context.Background(), deps)
		if got.Status != "unknown" || got.Reason != "git_unavailable" || got.DirtyStatus != "unknown" {
			t.Fatalf("workspace=%+v", got)
		}
	})
	t.Run("not repository", func(t *testing.T) {
		failure := &orientGitError{Err: &exec.ExitError{}, Diagnostic: "fatal: not a git repository"}
		deps := orientDependencies{getwd: func() (string, error) { return "/work", nil }, git: fakeOrientGit{orientGitKey("rev-parse", "--is-inside-work-tree"): {err: failure}}}
		got := inspectOrientWorkspace(context.Background(), deps)
		if got.Status != "not_repository" || got.Reason != "not_a_git_repository" {
			t.Fatalf("workspace=%+v", got)
		}
	})
}

func TestInspectOrientConfigurationUsesStaticExecutableHealth(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "codex")
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	cfg := `{"schema_version":1,"default_profile":"fleet","profiles":{"fleet":{"adapters":{"codex":{"executable":` + strconvQuote(executable) + `}},"route":{"this_host":"air","hosts":{"studio":"host-studio"},"placement":{"kind":"local"}}}}}`
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := defaultOrientDependencies()
	configuration, authorities, adapters := inspectOrientConfiguration(common{configPath: configPath}, deps)
	if configuration.Status != "ready" || configuration.Profile != "fleet" || configuration.Route.Status != "ready" || configuration.Route.ThisHost != "air" {
		t.Fatalf("configuration=%+v", configuration)
	}
	if len(authorities) != 2 || authorities[0].Kind != "native" || authorities[0].Health != "healthy" || authorities[1].Health != "unconfigured" {
		t.Fatalf("authorities=%+v", authorities)
	}
	if len(adapters) != 1 || adapters[0].Name != "codex" || adapters[0].Health != "healthy" || adapters[0].HealthBasis != "executable_present" {
		t.Fatalf("adapters=%+v", adapters)
	}
	if !strings.Contains(authorities[0].HealthBasis, "local_executable") {
		t.Fatalf("health basis does not delimit claim: %+v", authorities[0])
	}
}

func strconvQuote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestInspectOrientExecutionsMatchesOnlyCurrentWorkspace(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "state", "journal.db")
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	journal, err := store.Open(journalPath, store.Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "checkout")
	other := filepath.Join(root, "other")
	for _, cwd := range []*string{orientStringPointer(workspace), orientStringPointer(filepath.Join(workspace, "subdir")), orientStringPointer(other), nil} {
		value := orientTestExecution(now)
		value.CWD = cwd
		if _, _, err := journal.CreateExecution(context.Background(), value, contracts.MutationKey{}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	got := a.inspectOrientExecutions(context.Background(), common{journalPath: journalPath}, orientWorkspace{Status: "ready", CWD: workspace, WorktreeRoot: workspace}, 10)
	if got.JournalStatus != "ready" || got.Matched != 2 || len(got.Active) != 2 || len(got.Recent) != 2 || got.Unscoped != 1 {
		t.Fatalf("executions=%+v", got)
	}
	if got.Active[0].Labels == nil || got.Recent[0].Labels == nil {
		t.Fatalf("nil labels leaked into deterministic output: %+v", got)
	}
}

func orientTestExecution(now time.Time) model.Execution {
	return model.Execution{Authority: model.AuthorityNative, Adapter: "codex", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}}, Supersedes: []ids.ExecutionID{}, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}}
}

func orientStringPointer(value string) *string { return &value }

func TestOrientMissingStateIsVisibleAndReadOnly(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "missing-config.json")
	journalPath := filepath.Join(root, "missing-state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journalPath, "orient"}); code != 0 {
		t.Fatalf("exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var document struct {
		Result orientReport `json:"result"`
		Next   []struct {
			Mutates bool `json:"mutates"`
		} `json:"next_actions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Result.Configuration.Status != "unconfigured" || document.Result.Executions.JournalStatus != "unconfigured" {
		t.Fatalf("missing state was not classified: %s", stdout.String())
	}
	if len(document.Result.Executions.Active) != 0 || len(document.Result.Executions.Recent) != 0 {
		t.Fatalf("missing journal did not render empty arrays: %s", stdout.String())
	}
	for _, action := range document.Next {
		if action.Mutates {
			t.Fatalf("orient suggested mutating action: %s", stdout.String())
		}
	}
	if _, err := os.Stat(filepath.Dir(journalPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only orient created journal parent: %v", err)
	}
}

func TestParseOrient(t *testing.T) {
	if limit, problem := parseOrient([]string{"--limit", "25"}); problem != nil || limit != 25 {
		t.Fatalf("limit=%d problem=%v", limit, problem)
	}
	for _, args := range [][]string{{"--limit", "0"}, {"--limit"}, {"--unknown"}} {
		if _, problem := parseOrient(args); problem == nil {
			t.Fatalf("args=%v accepted", args)
		}
	}
}
