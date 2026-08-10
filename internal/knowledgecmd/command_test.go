package knowledgecmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunKnowledgeLifecycleAndContext(t *testing.T) {
	origin := t.TempDir()
	runGit(t, origin, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "runbook.md"), []byte("# Reliability\nrestart service safely\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, origin, "add", "runbook.md")
	runGit(t, origin, "-c", "user.email=fixture@example.test", "-c", "user.name=Fixture", "commit", "--quiet", "-m", "fixture")

	regFile := filepath.Join(t.TempDir(), "source.yaml")
	reg := "schema_version: 1\n" +
		"id: repo-amber-willow-orbit-tiger-harbor-gentle\n" +
		"slug: fixture-knowledge\n" +
		"mode: loose\n" +
		"remote:\n  provider: generic\n  url: " + origin + "\n  credential_mode: native_git\n" +
		"ref: refs/heads/main\nsubpath: .\nsensitivity: project-confidential\n" +
		"ingest:\n  include: [\"**/*.md\"]\n  exclude: [\"**/.git/**\"]\n  max_file_bytes: 1048576\n"
	if err := os.WriteFile(regFile, []byte(reg), 0600); err != nil {
		t.Fatal(err)
	}

	got, lines, err := Run(context.Background(), []string{"knowledge", "validate", "--source", regFile}, IO{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.(ValidateResult).Valid || len(lines) != 1 || !strings.HasPrefix(lines[0], "knowledge.validate ") {
		t.Fatalf("validate result: %#v %#v", got, lines)
	}

	checkout := filepath.Join(t.TempDir(), "checkout")
	got, _, err = Run(context.Background(), []string{"knowledge", "sync", "--source", regFile, "--checkout", checkout}, IO{})
	if err != nil {
		t.Fatal(err)
	}
	if got.(SyncResult).Commit == "" {
		t.Fatal("sync did not resolve commit")
	}

	bundle := filepath.Join(t.TempDir(), "bundle")
	got, _, err = Run(context.Background(), []string{"knowledge", "compile", "--source", regFile + "=" + checkout, "--output", bundle}, IO{})
	if err != nil {
		t.Fatal(err)
	}
	compiled := got.(CompileResult)
	if compiled.RecordCount == 0 || compiled.BundleRevision == "" {
		t.Fatalf("compile result: %#v", compiled)
	}
	if _, _, err := Run(context.Background(), []string{"knowledge", "compile", "--source", regFile + "=" + checkout, "--output", bundle}, IO{}); err == nil {
		t.Fatal("compile overwrote an existing output")
	}

	got, _, err = Run(context.Background(), []string{"knowledge", "verify", "--bundle", bundle}, IO{})
	if err != nil {
		t.Fatal(err)
	}
	if got.(VerifyResult).BundleRevision != compiled.BundleRevision {
		t.Fatalf("verify revision: %#v", got)
	}

	installed := filepath.Join(t.TempDir(), "installed")
	if _, _, err := Run(context.Background(), []string{"knowledge", "install", "--bundle", bundle, "--destination", installed}, IO{}); err != nil {
		t.Fatal(err)
	}
	renderPath := filepath.Join(t.TempDir(), "context.md")
	got, lines, err = Run(context.Background(), []string{"context", "--bundle", installed, "--task-kind", "reliability", "--query", "restart", "--render", renderPath, "--max", "1", "--max-bytes", "512"}, IO{Now: func() time.Time { return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	ctxResult := got.(ContextResult)
	if len(ctxResult.Matches) != 1 || ctxResult.RenderPath != renderPath || len(lines) != 2 {
		t.Fatalf("context result: %#v lines=%#v", ctxResult, lines)
	}
	raw, err := os.ReadFile(renderPath)
	if err != nil || !strings.Contains(string(raw), "restart service safely") {
		t.Fatalf("rendered context: %q (%v)", raw, err)
	}
	if encoded, err := json.Marshal(ctxResult); err != nil || strings.Contains(string(encoded), "restart service safely") {
		t.Fatalf("context result leaked record text: %s (%v)", encoded, err)
	}
}

func TestRunReadCommandsDoNotUseInjectedGit(t *testing.T) {
	called := false
	git := fakeGit{run: func() { called = true }}
	if _, _, err := Run(context.Background(), []string{"knowledge", "validate", "--source", filepath.Join(t.TempDir(), "missing")}, IO{Git: git}); err == nil {
		t.Fatal("missing source unexpectedly validated")
	}
	if called {
		t.Fatal("validate invoked Git")
	}
}

type fakeGit struct{ run func() }

func (f fakeGit) Run(context.Context, string, ...string) ([]byte, error) {
	if f.run != nil {
		f.run()
	}
	return nil, os.ErrNotExist
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}
