package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestPromptPathsResolveRootAliasesAndRejectEscapes(t *testing.T) {
	root := testutil.TempDir(t)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(testutil.TempDir(t), "cwd-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(filepath.Join(realRoot, "task.md"), []byte("private prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "task.md"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(realRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realRoot, "task.md"), filepath.Join(realRoot, "linked.md")); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"run", "delegate", "continue", "steer", "dispatch", "fanout"} {
		for _, test := range []struct {
			name, cwd, path string
			allowed         bool
		}{
			{"canonical_cwd_alias_prompt", realRoot, filepath.Join(alias, "task.md"), true},
			{"alias_cwd_canonical_prompt", alias, filepath.Join(realRoot, "task.md"), true},
			{"alias_cwd_absolute_prompt", alias, filepath.Join(alias, "task.md"), true},
			{"alias_cwd_relative_prompt", alias, "task.md", true},
			{"escaping_directory_link", alias, filepath.Join(alias, "escape", "task.md"), false},
			{"escaping_relative_directory_link", realRoot, filepath.Join("escape", "task.md"), false},
			{"symlink_leaf", realRoot, "linked.md", false},
		} {
			t.Run(command+"/"+test.name, func(t *testing.T) {
				a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
				payload, problem := a.loadPromptForCommand(command, runOptions{cwd: test.cwd, promptFile: test.path, promptDelivery: "argv"})
				if test.allowed {
					if problem != nil || payload == nil || string(payload.Bytes) != "private prompt" || payload.Path != filepath.Join(realRoot, "task.md") {
						t.Fatalf("payload=%+v problem=%v", payload, problem)
					}
				} else if problem == nil || problem.Code != output.CodeAuthorizationDenied || payload != nil {
					t.Fatalf("payload=%+v problem=%v", payload, problem)
				}
			})
		}
	}
}

func TestFanoutPromptCacheUsesCanonicalRootAliases(t *testing.T) {
	root := testutil.TempDir(t)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(testutil.TempDir(t), "cwd-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(filepath.Join(root, "task.md"), []byte("private prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	manifest := fanoutManifest{SchemaVersion: 1, PromptFile: filepath.Join(alias, "task.md"), Children: []fanoutChild{{PromptFile: filepath.Join(realRoot, "task.md")}}}
	prompts, shared, problem := a.loadFanoutPrompts(manifest, realRoot)
	if problem != nil {
		t.Fatal(problem)
	}
	if &prompts[0].Bytes[0] != &shared.Bytes[0] {
		t.Fatal("root aliases duplicated prompt storage")
	}
	if err := os.Symlink("task.md", filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	manifest.Children[0].PromptFile = filepath.Join(alias, "linked.md")
	if _, _, problem := a.loadFanoutPrompts(manifest, realRoot); problem == nil || problem.Code != output.CodeAuthorizationDenied {
		t.Fatalf("cached prompt bypassed symlink check: %v", problem)
	}
}

func TestDelegateAndContinuePromptFilesUnderAliasedCWD(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	realRoot, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(testutil.TempDir(t), "cwd-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skip(err)
	}
	for _, name := range []string{"task.md", "feedback.md"} {
		if err := os.WriteFile(filepath.Join(realRoot, name), []byte("private instruction"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	code, out := f.command(t, "", "delegate", "--cwd", alias, "--request-file", f.request, "--prompt-file", filepath.Join(alias, "task.md"), "--wait")
	if code != 0 {
		t.Fatalf("delegate: %d %s", code, out)
	}
	first := resultID(t, out)
	t.Chdir(alias)
	code, out = f.command(t, "", "continue", first, "--request-key", "aliased-follow-up", "--prompt-file", filepath.Join(alias, "feedback.md"), "--wait")
	if code != 0 {
		t.Fatalf("continue: %d %s", code, out)
	}
	second := resultID(t, out)
	outside := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "outside.md"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.md"), filepath.Join(alias, "escape.md")); err != nil {
		t.Fatal(err)
	}
	code, out = f.command(t, "", "continue", second, "--request-key", "escaping-follow-up", "--prompt-file", filepath.Join(alias, "escape.md"), "--wait")
	if code != 7 || f.launches(t) != 2 {
		t.Fatalf("escape: %d %s", code, out)
	}
}
