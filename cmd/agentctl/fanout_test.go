package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func writeFanoutTestFile(t *testing.T, root, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFanoutTestManifest(t *testing.T, root string, m fanoutManifest) string {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return writeFanoutTestFile(t, root, "fanout.json", string(body), 0o600)
}

func TestFanoutPreflightsWholeBatchBeforeAnyTaskLaunch(t *testing.T) {
	for _, invalid := range []string{"missing_executable", "bad_cwd", "unsupported_invocation"} {
		t.Run(invalid, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "launched")
			writeFanoutTestFile(t, root, "task.md", "bounded task", 0o600)
			script := writeFanoutTestFile(t, root, "agent", fmt.Sprintf("#!/bin/sh\n: > %q\nprintf '%%s\\n' '{\"type\":\"result\",\"status\":\"completed\",\"result\":\"ok\"}'\n", marker), 0o700)
			bad := fanoutChild{Adapter: "generic-process", Argv: []string{script}}
			switch invalid {
			case "missing_executable":
				bad.Argv[0] = filepath.Join(root, "missing-agent")
			case "bad_cwd":
				bad.CWD = filepath.Join(root, "missing-directory")
			case "unsupported_invocation":
				bad.Adapter = "codex" // No native exec --json.
			}
			manifest := writeFanoutTestManifest(t, root, fanoutManifest{SchemaVersion: 1, PromptFile: "task.md", Concurrency: 1, Children: []fanoutChild{{Adapter: "generic-process", Argv: []string{script}}, bad}})
			journal := filepath.Join(root, "state", "journal.db")
			a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
			problem := a.fanout(context.Background(), output.Renderer{Mode: output.JSON, Writer: &bytes.Buffer{}}, common{journalPath: journal}, []string{"--manifest", manifest})
			if problem == nil {
				t.Fatal("invalid batch succeeded")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("task launched during preflight: %v", err)
			}
			if _, err := os.Stat(filepath.Dir(journal)); !os.IsNotExist(err) {
				t.Fatalf("preflight created journal state: %v", err)
			}
			result, ok := problem.Details["fanout"].(fanoutResult)
			if !ok || result.Success || len(result.Children) != 2 {
				t.Fatalf("missing per-child preflight report: %#v", problem)
			}
			for _, child := range result.Children {
				if child.LaunchAttempted || child.Recorded {
					t.Fatalf("false launch evidence: %#v", child)
				}
			}
		})
	}
}

func TestFanoutDistinctTasksPreserveArgvAndPersistLabels(t *testing.T) {
	root := t.TempDir()
	writeFanoutTestFile(t, root, "review.md", "PRIVATE_REVIEW_TASK", 0o600)
	writeFanoutTestFile(t, root, "implement.md", "PRIVATE_IMPLEMENT_TASK", 0o600)
	script := writeFanoutTestFile(t, root, "agent", "#!/bin/sh\n[ \"$1\" = '--native-flag' ] || exit 9\nif [ \"$2\" = stdin ]; then prompt=$(cat); else prompt=$3; fi\nprintf '{\"type\":\"result\",\"status\":\"completed\",\"result\":\"%s\"}\\n' \"$prompt\"\n", 0o700)
	manifest := writeFanoutTestManifest(t, root, fanoutManifest{SchemaVersion: 1, Labels: []string{"batch-one"}, Children: []fanoutChild{
		{Name: "review", Adapter: "generic-process", PromptFile: "review.md", Labels: []string{"batch-one", "review"}, Argv: []string{script, "--native-flag", "argv"}},
		{Name: "implement", Adapter: "generic-process", PromptFile: "implement.md", PromptDelivery: "stdin", Labels: []string{"implement"}, Argv: []string{script, "--native-flag", "stdin"}},
	}})
	journalPath := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journalPath, "fanout", "--manifest", manifest}); code != 0 {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	var doc struct {
		Result fanoutResult `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Result.Success || doc.Result.Prompt != nil || len(doc.Result.Children) != 2 {
		t.Fatalf("result=%#v", doc.Result)
	}
	if strings.Contains(stdout.String(), "PRIVATE_") {
		t.Fatal("fanout output leaked prompt text")
	}
	for i, child := range doc.Result.Children {
		if !child.Recorded || !child.LaunchAttempted || child.State != model.StateCompleted || child.Error != nil {
			t.Fatalf("child=%#v", child)
		}
		if child.Name != []string{"review", "implement"}[i] || child.Prompt["delivery"] != []string{"argv", "stdin"}[i] {
			t.Fatalf("metadata=%#v", child)
		}
		j, problem := a.openRead(common{journalPath: journalPath})
		if problem != nil {
			t.Fatal(problem)
		}
		id, err := fanoutExecutionID(child.ExecutionID)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := j.GetExecution(context.Background(), id)
		j.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(execution.Labels, []string{"batch-one", child.Name}) {
			t.Fatalf("labels=%v", execution.Labels)
		}
		stdout.Reset()
		if code := a.run(context.Background(), []string{"--journal", journalPath, "result", child.ExecutionID, "--content"}); code != 0 {
			t.Fatalf("result exit=%d output=%s", code, stdout.String())
		}
		if stdout.String() != []string{"PRIVATE_REVIEW_TASK", "PRIVATE_IMPLEMENT_TASK"}[i] {
			t.Fatalf("wrong prompt delivered: %s", stdout.String())
		}
	}
}

func TestFanoutPromptCacheSharesBytesNotDeliveryMetadata(t *testing.T) {
	root := t.TempDir()
	writeFanoutTestFile(t, root, "task.md", "prompt", 0o600)
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	m := fanoutManifest{SchemaVersion: 1, PromptFile: "task.md", Children: []fanoutChild{{PromptDelivery: "argv"}, {PromptFile: "./task.md", PromptDelivery: "stdin"}}}
	prompts, shared, problem := a.loadFanoutPrompts(m, root)
	if problem != nil {
		t.Fatal(problem)
	}
	if prompts[0].Delivery != "argv" || prompts[1].Delivery != "stdin" || shared.Delivery != "argv" {
		t.Fatal("delivery metadata was mutated")
	}
	if &prompts[0].Bytes[0] != &prompts[1].Bytes[0] || &shared.Bytes[0] != &prompts[0].Bytes[0] {
		t.Fatal("same prompt was materialized more than once")
	}
}

func TestFanoutPromptCacheCollapsesAbsolutePath(t *testing.T) {
	root := t.TempDir()
	writeFanoutTestFile(t, root, "task.md", "prompt", 0o600)
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	m := fanoutManifest{SchemaVersion: 1, PromptFile: "task.md", Children: []fanoutChild{
		{PromptDelivery: "argv"},
		{PromptFile: filepath.Join(root, "task.md"), PromptDelivery: "stdin"},
	}}
	prompts, shared, problem := a.loadFanoutPrompts(m, root)
	if problem != nil {
		t.Fatal(problem)
	}
	if prompts[0].Delivery != "argv" || prompts[1].Delivery != "stdin" {
		t.Fatal("delivery metadata was mutated")
	}
	if &prompts[0].Bytes[0] != &prompts[1].Bytes[0] || &shared.Bytes[0] != &prompts[0].Bytes[0] {
		t.Fatal("same confined path was materialized more than once")
	}
}

func TestFanoutPromptSymlinkIsStillRejected(t *testing.T) {
	root := t.TempDir()
	writeFanoutTestFile(t, root, "task.md", "prompt", 0o600)
	if err := os.Symlink("task.md", filepath.Join(root, "alias.md")); err != nil {
		t.Skip(err)
	}
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	m := fanoutManifest{SchemaVersion: 1, PromptFile: "task.md", Children: []fanoutChild{
		{PromptDelivery: "argv"},
		{PromptFile: "alias.md", PromptDelivery: "stdin"},
	}}
	if _, _, problem := a.loadFanoutPrompts(m, root); problem == nil {
		t.Fatal("symlink prompt was accepted")
	}
}

func TestFanoutLoadsAllPromptsBeforeLaunching(t *testing.T) {
	root := t.TempDir()
	writeFanoutTestFile(t, root, "first.md", "first", 0o600)
	second := writeFanoutTestFile(t, root, "second.md", "ORIGINAL_SECOND_TASK", 0o600)
	mutate := writeFanoutTestFile(t, root, "mutate-agent", fmt.Sprintf("#!/bin/sh\nprintf 'CHANGED' > %q\nprintf '%%s\\n' '{\"type\":\"result\",\"status\":\"completed\",\"result\":\"first\"}'\n", second), 0o700)
	echo := writeFanoutTestFile(t, root, "echo-agent", "#!/bin/sh\nprintf '{\"type\":\"result\",\"status\":\"completed\",\"result\":\"%s\"}\\n' \"$1\"\n", 0o700)
	manifest := writeFanoutTestManifest(t, root, fanoutManifest{SchemaVersion: 1, Concurrency: 1, Children: []fanoutChild{{Adapter: "generic-process", PromptFile: "first.md", Argv: []string{mutate}}, {Adapter: "generic-process", PromptFile: "second.md", Argv: []string{echo}}}})
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journal, "fanout", "--manifest", manifest}); code != 0 {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	var doc struct {
		Result fanoutResult `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "result", doc.Result.Children[1].ExecutionID, "--content"}); code != 0 || stdout.String() != "ORIGINAL_SECOND_TASK" {
		t.Fatalf("mutable prompt used: exit=%d output=%s", code, stdout.String())
	}
}

func TestFanoutFailureSkipsQueuedWorkButDoesNotInventCancellationState(t *testing.T) {
	root := t.TempDir()
	writeFanoutTestFile(t, root, "task.md", "task", 0o600)
	fail := writeFanoutTestFile(t, root, "fail-agent", "#!/bin/sh\nexit 7\n", 0o700)
	marker := filepath.Join(root, "second-started")
	second := writeFanoutTestFile(t, root, "second-agent", fmt.Sprintf("#!/bin/sh\n: > %q\nprintf '%%s\\n' '{\"type\":\"result\",\"status\":\"completed\",\"result\":\"ok\"}'\n", marker), 0o700)
	manifest := writeFanoutTestManifest(t, root, fanoutManifest{SchemaVersion: 1, PromptFile: "task.md", Concurrency: 1, Children: []fanoutChild{{Adapter: "generic-process", Argv: []string{fail}}, {Adapter: "generic-process", Argv: []string{second}}}})
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	problem := a.fanout(context.Background(), output.Renderer{Mode: output.JSON, Writer: &bytes.Buffer{}}, common{journalPath: filepath.Join(root, "state", "journal.db")}, []string{"--manifest", manifest, "--fail-fast"})
	if problem == nil {
		t.Fatal("failed batch succeeded")
	}
	result := problem.Details["fanout"].(fanoutResult)
	if !result.Children[0].LaunchAttempted || !result.Children[0].Recorded || result.Children[0].State != model.StateFailed {
		t.Fatalf("failure evidence=%#v", result.Children[0])
	}
	if result.Children[1].LaunchAttempted || result.Children[1].Recorded || result.Children[1].State != "" || result.Children[1].Error == nil {
		t.Fatalf("skipped evidence=%#v", result.Children[1])
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("queued child launched: %v", err)
	}
}

func TestFanoutTimeoutPreservesRecordedTerminalState(t *testing.T) {
	root := t.TempDir()
	writeFanoutTestFile(t, root, "task.md", "task", 0o600)
	script := writeFanoutTestFile(t, root, "agent", "#!/bin/sh\nexec sleep 30\n", 0o700)
	manifest := writeFanoutTestManifest(t, root, fanoutManifest{SchemaVersion: 1, PromptFile: "task.md", Children: []fanoutChild{{Adapter: "generic-process", Timeout: "200ms", Argv: []string{script}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	problem := a.fanout(ctx, output.Renderer{Mode: output.JSON, Writer: &bytes.Buffer{}}, common{journalPath: filepath.Join(root, "state", "journal.db")}, []string{"--manifest", manifest})
	if problem == nil {
		t.Fatal("timed-out batch succeeded")
	}
	result, ok := problem.Details["fanout"].(fanoutResult)
	if !ok || len(result.Children) != 1 {
		t.Fatalf("no child evidence: %#v", problem)
	}
	child := result.Children[0]
	if !child.Recorded || !child.LaunchAttempted || child.State != model.StateCancelled || child.Error == nil {
		t.Fatalf("lost timeout state: %#v", child)
	}
}

func TestFanoutPlanDoesNotCreateJournal(t *testing.T) {
	root := t.TempDir()
	writeFanoutTestFile(t, root, "task.md", "task", 0o600)
	marker := filepath.Join(root, "started")
	script := writeFanoutTestFile(t, root, "agent", fmt.Sprintf("#!/bin/sh\n: > %q\n", marker), 0o700)
	manifest := writeFanoutTestManifest(t, root, fanoutManifest{SchemaVersion: 1, PromptFile: "task.md", Children: []fanoutChild{{Adapter: "generic-process", Argv: []string{script}}}})
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journal, "fanout", "--plan", "--manifest", manifest}); code != 0 {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	for _, path := range []string{marker, filepath.Dir(journal)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("plan side effect %s: %v", path, err)
		}
	}
}

func TestFanoutExistingIDRejectsWholeBatchWithoutReplay(t *testing.T) {
	root := t.TempDir()
	writeFanoutTestFile(t, root, "task.md", "task", 0o600)
	script := writeFanoutTestFile(t, root, "agent", "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"status\":\"completed\",\"result\":\"ok\"}'\n", 0o700)
	m := fanoutManifest{SchemaVersion: 1, PromptFile: "task.md", Children: []fanoutChild{{Adapter: "generic-process", Argv: []string{script}}}}
	manifest := writeFanoutTestManifest(t, root, m)
	c := common{journalPath: filepath.Join(root, "state", "journal.db")}
	var stdout bytes.Buffer
	a := testApp(&stdout, &bytes.Buffer{})
	if problem := a.fanout(context.Background(), output.Renderer{Mode: output.JSON, Writer: &stdout}, c, []string{"--manifest", manifest}); problem != nil {
		t.Fatal(problem)
	}
	var doc struct {
		Result fanoutResult `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "new-task-started")
	fresh := writeFanoutTestFile(t, root, "fresh-agent", fmt.Sprintf("#!/bin/sh\n: > %q\n", marker), 0o700)
	m.Children[0].ExecutionID = doc.Result.Children[0].ExecutionID
	m.Children = append([]fanoutChild{{Adapter: "generic-process", Argv: []string{fresh}}}, m.Children...)
	manifest = writeFanoutTestManifest(t, root, m)
	stdout.Reset()
	problem := a.fanout(context.Background(), output.Renderer{Mode: output.JSON, Writer: &stdout}, c, []string{"--manifest", manifest})
	if problem == nil || problem.Code != output.CodeUsage {
		t.Fatalf("expected preflight failure, got %#v", problem)
	}
	result := problem.Details["fanout"].(fanoutResult)
	first, existing := result.Children[0], result.Children[1]
	if first.LaunchAttempted || first.Recorded || first.State != "" {
		t.Fatalf("new task was admitted: %#v", first)
	}
	if existing.LaunchAttempted || !existing.Recorded || existing.State != model.StateCompleted || existing.Error == nil || existing.Error.Code != output.CodeConflict {
		t.Fatalf("existing execution not reported as conflict: %#v", existing)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("new task launched despite preflight conflict: %v", err)
	}
}
