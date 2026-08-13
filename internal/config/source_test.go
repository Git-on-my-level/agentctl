package config

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Agentctl Test", "GIT_AUTHOR_EMAIL=agentctl@example.invalid", "GIT_COMMITTER_NAME=Agentctl Test", "GIT_COMMITTER_EMAIL=agentctl@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func sourceFixture(t *testing.T) (remote, author, configPath, checkout string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	if err := os.Mkdir(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, remote, "init", "--bare", "--quiet")
	author = filepath.Join(root, "author")
	if err := os.Mkdir(author, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, author, "init", "--quiet", "--initial-branch", "main")
	if err := os.WriteFile(filepath.Join(author, defaultSourceBundle), []byte(validBundle+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, author, "add", defaultSourceBundle)
	runGitTest(t, author, "commit", "--quiet", "-m", "initial")
	runGitTest(t, author, "remote", "add", "origin", remote)
	runGitTest(t, author, "push", "--quiet", "-u", "origin", "main")
	configPath = filepath.Join(root, "runtime", "config.json")
	checkout = filepath.Join(root, "managed", "config-source")
	return remote, author, configPath, checkout
}

func TestSourceInitUpdateAndStatusAreDeterministic(t *testing.T) {
	remote, author, configPath, checkout := sourceFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	update, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, Ref: "main", CheckoutPath: checkout}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !update.Changed || !update.Status.Configured || !update.Status.InSync || update.Status.AppliedCommit == "" {
		t.Fatalf("unexpected init: %#v", update)
	}
	info, err := os.Stat(configPath)
	if err != nil || info.Mode().Perm() != ConfigFileMode {
		t.Fatalf("live config mode=%v err=%v", info.Mode().Perm(), err)
	}
	statePath, err := SourceStatePath(configPath)
	if err != nil {
		t.Fatal(err)
	}
	stateInfo, err := os.Stat(statePath)
	if err != nil || stateInfo.Mode().Perm() != ConfigFileMode {
		t.Fatalf("state mode=%v err=%v", stateInfo.Mode().Perm(), err)
	}

	if err := os.WriteFile(filepath.Join(author, "README.md"), []byte("private config source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, author, "add", "README.md")
	runGitTest(t, author, "commit", "--quiet", "-m", "docs")
	runGitTest(t, author, "push", "--quiet", "origin", "main")
	second, err := UpdateSource(ctx, configPath, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Changed || !second.Status.InSync || second.Status.AppliedCommit == update.Status.AppliedCommit {
		t.Fatalf("unexpected update: %#v", second)
	}
	third, err := UpdateSource(ctx, configPath, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if third.Changed || !third.Status.InSync {
		t.Fatalf("idempotent update=%#v", third)
	}
}

func TestSourceUpdateFailsClosedOnLiveOrCheckoutDrift(t *testing.T) {
	remote, _, configPath, checkout := sourceFixture(t)
	ctx := context.Background()
	if _, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(validBundle), ConfigFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateSource(ctx, configPath, time.Now()); err == nil || !strings.Contains(err.Error(), "live config drifted") {
		t.Fatalf("live drift error=%v", err)
	}
	// Restore the exact materialized bytes, then introduce checkout drift.
	statePath, _ := SourceStatePath(configPath)
	state, err := LoadSourceState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := LoadBundle(filepath.Join(checkout, defaultSourceBundle))
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(configPath, MaterializeBundle(bundle), true); err != nil {
		t.Fatal(err)
	}
	state.LiveSHA256, err = fileSHA256(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSourceState(statePath, state, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "local.txt"), []byte("drift"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateSource(ctx, configPath, time.Now()); err == nil || !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("checkout drift error=%v", err)
	}
}

func TestRestoreSourceRepairsOnlyLiveConfigWithoutFetching(t *testing.T) {
	remote, _, configPath, checkout := sourceFixture(t)
	ctx := context.Background()
	initial, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, ' '), ConfigFileMode); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreSource(ctx, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Changed || !restored.Status.InSync || restored.Status.AppliedCommit != initial.Status.AppliedCommit {
		t.Fatalf("unexpected restore: %#v", restored)
	}
}

func TestSourcePlanAndUnconfiguredStatusDoNotCreatePaths(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "runtime", "config.json")
	checkout := filepath.Join(root, "checkout")
	plan, err := PlanSource(configPath, SourceSpec{Remote: "git@github.com:example/private.git", CheckoutPath: checkout}, "init")
	if err != nil {
		t.Fatal(err)
	}
	if plan.PlanEffects.Mutates || plan.PlanEffects.NetworkAccess || !plan.ApplyEffects.Mutates || !plan.ApplyEffects.NetworkAccess || plan.RemoteValidated {
		t.Fatalf("plan=%#v", plan)
	}
	if _, err := os.Stat(filepath.Dir(configPath)); !os.IsNotExist(err) {
		t.Fatalf("plan created config parent: %v", err)
	}
	status, err := SourceStatusReadOnly(context.Background(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.Configured || status.InSync {
		t.Fatalf("status=%#v", status)
	}
	if _, err := os.Stat(filepath.Dir(configPath)); !os.IsNotExist(err) {
		t.Fatalf("status created config parent: %v", err)
	}
}

func TestSourceInitInvalidBundleLeavesRetryableDestination(t *testing.T) {
	remote, author, configPath, checkout := sourceFixture(t)
	if err := os.WriteFile(filepath.Join(author, defaultSourceBundle), []byte(`{"schema_version":1,"profiles":{},"unknown":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, author, "add", defaultSourceBundle)
	runGitTest(t, author, "commit", "--quiet", "-m", "invalid")
	runGitTest(t, author, "push", "--quiet", "origin", "main")
	if _, err := InitSource(context.Background(), configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now()); err == nil {
		t.Fatal("invalid source unexpectedly initialized")
	}
	if _, err := os.Lstat(checkout); !os.IsNotExist(err) {
		t.Fatalf("failed init poisoned checkout destination: %v", err)
	}
	if _, err := os.Lstat(configPath); !os.IsNotExist(err) {
		t.Fatalf("failed init created live config: %v", err)
	}
}

func TestSourceUpdateRejectsNonFastForwardAndPreservesAppliedState(t *testing.T) {
	remote, author, configPath, checkout := sourceFixture(t)
	ctx := context.Background()
	initial, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, author, "checkout", "--quiet", "--orphan", "replacement")
	runGitTest(t, author, "rm", "--quiet", "-rf", ".")
	if err := os.WriteFile(filepath.Join(author, defaultSourceBundle), []byte(strings.Replace(validBundle, "/opt/bin/codex", "/different/codex", 1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, author, "add", defaultSourceBundle)
	runGitTest(t, author, "commit", "--quiet", "-m", "replacement history")
	runGitTest(t, author, "push", "--quiet", "--force", "origin", "HEAD:main")
	if _, err := UpdateSource(ctx, configPath, time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-fast-forward error=%v, want conflict", err)
	}
	status, err := SourceStatusReadOnly(ctx, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !status.InSync || status.AppliedCommit != initial.Status.AppliedCommit || status.CheckoutCommit != initial.Status.AppliedCommit {
		t.Fatalf("non-fast-forward changed applied state: %#v", status)
	}
}
