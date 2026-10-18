package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

func TestInferConfiguredAdapter(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "claude-2")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "native-alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	configured := map[string]config.Adapter{"claude-code": {Executable: path}}
	for _, executable := range []string{path, "claude-2", "/another/path/claude-2", alias} {
		got, problem := inferConfiguredAdapter(executable, configured)
		if problem != nil || got != "claude" {
			t.Fatalf("executable=%s adapter=%s problem=%v", executable, got, problem)
		}
	}
	// A resolved path outranks a competing basename. Alias entries collapse.
	configured["codex"] = config.Adapter{Executable: "/different/claude-2"}
	configured["claude"] = configured["claude-code"]
	for i := 0; i < 50; i++ {
		got, problem := inferConfiguredAdapter(path, configured)
		if problem != nil || got != "claude" {
			t.Fatalf("path precedence adapter=%s problem=%v", got, problem)
		}
		_, problem = inferConfiguredAdapter("/unresolved/claude-2", configured)
		if problem == nil || problem.Details["diagnostic_code"] != "run_adapter_ambiguous" || !reflect.DeepEqual(problem.Details["candidates"], []string{"claude", "codex"}) {
			t.Fatalf("basename conflict=%v", problem)
		}
	}
	configured["codex"] = configured["claude-code"]
	if _, problem := inferConfiguredAdapter(path, configured); problem == nil {
		t.Fatal("same-path ambiguity accepted")
	}
	if got, problem := inferConfiguredAdapter("opencode", nil); problem != nil || got != "opencode" {
		t.Fatalf("built-in fallback=%s problem=%v", got, problem)
	}
}

func TestRunConfiguredClaudeExecutableStoresNativeResult(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "claude-2")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '2.1.0 (Claude Code)'; exit 0; fi\n" + delegateSuccessScript + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	cfg := config.Config{SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{
		"test":  {Adapters: map[string]config.Adapter{"claude-code": {Executable: path}}, Delegation: &config.DelegationPolicy{UnattendedCodingPermissions: true}},
		"other": {Adapters: map[string]config.Adapter{"codex": {Executable: path}}},
	}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ profile, explicit, want string }{{"test", "", "claude"}, {"other", "", "codex"}, {"test", "generic-process", "generic-process"}} {
		args := []string{"--", path, "--print", "--verbose", "--output-format", "stream-json", "task"}
		if tc.explicit != "" {
			args = append([]string{"--adapter", tc.explicit}, args...)
		}
		opts, problem := parseRunWithConfig(args, common{configPath: configPath, profile: tc.profile})
		if problem != nil || opts.adapter != tc.want || !reflect.DeepEqual(opts.argv, []string{path, "--print", "--verbose", "--output-format", "stream-json", "task"}) {
			t.Fatalf("profile=%s opts=%#v problem=%v", tc.profile, opts, problem)
		}
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	journal := filepath.Join(root, "state", "journal.db")
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journal, "run", "--", path, "--print", "--verbose", "--output-format", "stream-json", "task"}); code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var doc struct{ Result model.Execution }
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Result.Adapter != "claude-code" || doc.Result.State != model.StateCompleted {
		t.Fatalf("execution=%#v", doc.Result)
	}
	// Fanout must report and launch the same inferred adapter as expert run.
	if err := os.WriteFile(filepath.Join(root, "task.md"), []byte("task"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "fanout.json")
	data, err = json.Marshal(map[string]any{"schema_version": 1, "prompt_file": "task.md", "children": []any{map[string]any{"argv": []string{path, "--print", "--verbose", "--output-format", "stream-json"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journal, "fanout", "--manifest", manifest, "--plan"}); code != 0 {
		t.Fatalf("fanout exit=%d output=%s", code, stdout.String())
	}
	var fanoutDoc struct{ Result fanoutResult }
	if err := json.Unmarshal(stdout.Bytes(), &fanoutDoc); err != nil {
		t.Fatal(err)
	}
	if len(fanoutDoc.Result.Children) != 1 || fanoutDoc.Result.Children[0].Adapter != "claude" {
		t.Fatalf("fanout=%s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "result", doc.Result.ID.String(), "--content"}); code != 0 || stdout.String() != "verified answer" {
		t.Fatalf("result exit=%d output=%s", code, stdout.String())
	}
}
