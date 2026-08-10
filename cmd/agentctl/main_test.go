package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func testApp(stdout, stderr *bytes.Buffer) *app {
	return &app{stdout: stdout, stderr: stderr, getenv: func(string) string { return "" }, now: time.Now}
}

func TestAdapterProbeErrorsPreserveStableClassification(t *testing.T) {
	tests := []struct {
		adapterCode adapter.ErrorCode
		want        output.Code
	}{
		{adapter.ErrDependencyUnavailable, output.CodeDependencyUnavailable},
		{adapter.ErrAuthenticationRequired, output.CodeAuthenticationRequired},
		{adapter.ErrCapabilityUnavailable, output.CodeCapabilityUnavailable},
		{adapter.ErrTimeout, output.CodeTimeout},
		{adapter.ErrUsage, output.CodeUsage},
	}
	for _, test := range tests {
		err := &adapter.AdapterError{Code: test.adapterCode, Message: "probe fixture"}
		if got := mapAdapterError("adapter probe failed", err).Code; got != test.want {
			t.Fatalf("adapter code %s mapped to %s; want %s", test.adapterCode, got, test.want)
		}
	}
}

func TestSubscribeCancelRejectsInvalidIDBeforeCreatingJournal(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "subscribe", "cancel", "not-a-subscription-id"}); code != 2 {
		t.Fatalf("cancel exit=%d output=%s", code, stdout.String())
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("invalid mutation created journal: %v", err)
	}
}

func TestDoctorRequiresProfileSelectionWhenConfigHasNoDefault(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"schema_version":1,"profiles":{"local":{"adapters":{"generic":{"executable":"/bin/echo"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--config", configPath, "doctor"}); code != 3 || !strings.Contains(stdout.String(), `"code":"not_found"`) {
		t.Fatalf("doctor exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--config", configPath, "--profile", "local", "doctor"}); code != 0 {
		t.Fatalf("explicit doctor exit=%d output=%s", code, stdout.String())
	}
}

func TestHelpSideEffectClassesReflectOptionalWrites(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "help"}); code != 0 {
		t.Fatalf("help exit=%d output=%s", code, stdout.String())
	}
	var doc struct {
		Result struct {
			Commands []map[string]any `json:"commands"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	classes := map[string]string{}
	for _, command := range doc.Result.Commands {
		classes[command["name"].(string)] = command["side_effect_class"].(string)
	}
	if classes["attach"] != "read_only" || classes["context"] != "local_operational_write" {
		t.Fatalf("unexpected side-effect classes: %#v", classes)
	}
}

func TestParseCommonPreservesEveryArgumentAfterDelimiter(t *testing.T) {
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	common, rest, err := a.parseCommon([]string{"--output", "json", "run", "--adapter", "generic-process", "--", "tool", "--output", "native", "--profile", "child"})
	if err != nil {
		t.Fatal(err)
	}
	if common.mode != "json" {
		t.Fatalf("mode=%s", common.mode)
	}
	want := []string{"run", "--adapter", "generic-process", "--", "tool", "--output", "native", "--profile", "child"}
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("rest=%q want=%q", rest, want)
	}
}

func TestParseCommonLeavesKnowledgeBundleOutputForSubcommand(t *testing.T) {
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	common, rest, err := a.parseCommon([]string{"--output", "json", "knowledge", "compile", "--source", "source.json=/checkout", "--output", "/bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if common.mode != output.JSON {
		t.Fatalf("mode=%s", common.mode)
	}
	want := []string{"knowledge", "compile", "--source", "source.json=/checkout", "--output", "/bundle"}
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("rest=%q want=%q", rest, want)
	}
}

func TestTopLevelKnowledgeCompileKeepsBundleOutput(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "knowledge")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "runbook.md"), []byte("# Runbook\ncheck the durable event cursor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", "runbook.md")
	runTestGit(t, repository, "-c", "user.email=fixture@example.test", "-c", "user.name=Fixture", "commit", "--quiet", "-m", "fixture")
	source := filepath.Join(root, "source.yaml")
	registration := "schema_version: 1\n" +
		"id: repo-amber-willow-orbit-tiger-harbor-gentle\nslug: fixture-knowledge\nmode: loose\n" +
		"remote:\n  provider: generic\n  url: " + repository + "\n  credential_mode: native_git\n" +
		"ref: refs/heads/main\nsubpath: .\nsensitivity: project-confidential\n" +
		"ingest:\n  include: [\"**/*.md\"]\n  exclude: [\"**/.git/**\"]\n  max_file_bytes: 1048576\n"
	if err := os.WriteFile(source, []byte(registration), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "bundle")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"--output", "json", "knowledge", "compile", "--source", source + "=" + repository, "--output", bundle})
	if code != 0 {
		t.Fatalf("compile exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(bundle, "manifest.json")); err != nil {
		t.Fatalf("bundle was not created: %v", err)
	}
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, output)
	}
}

func TestRouteExplainJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"route", "explain", "--model-family", "gpt", "--needs-pr", "--output", "json"})
	if code != 0 {
		t.Fatalf("exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var doc struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.OK || doc.Result["lifecycle"] != "multica" || doc.Result["adapter"] != "codex" {
		t.Fatalf("doc=%#v", doc)
	}
}

func TestNativeRunSupervisesToTerminalAndRedactsPrivateSource(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	journal := filepath.Join(t.TempDir(), "state", "journal.db")
	native := `{"type":"result","status":"completed","result":{"summary":"must-not-enter-journal"}}`
	code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--adapter", "generic-process", "--", "/bin/echo", native})
	if code != 0 {
		t.Fatalf("exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var doc struct {
		OK     bool            `json:"ok"`
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.OK || doc.Result.State != model.StateCompleted {
		t.Fatalf("doc=%#v", doc)
	}
	if len(doc.Result.SourceBindings) != 1 || doc.Result.SourceBindings[0].OpaqueID != nil {
		t.Fatalf("normal output exposed opaque binding: %#v", doc.Result.SourceBindings)
	}
	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("must-not-enter-journal")) {
		t.Fatal("raw native result leaked into journal")
	}
}

func TestNativeRunWithPreallocatedIDReleasesJournalWhileRunning(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "state", "journal.db")
	script := filepath.Join(root, "slow-agent")
	contents := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"status\",\"status\":\"running\"}'\nsleep 2\nprintf '%s\\n' '{\"type\":\"result\",\"status\":\"completed\"}'\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	rawExecutionID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	executionID, err := ids.ParseExecutionID(rawExecutionID.String())
	if err != nil {
		t.Fatal(err)
	}
	var runOut, runErr bytes.Buffer
	runner := testApp(&runOut, &runErr)
	done := make(chan int, 1)
	go func() {
		done <- runner.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "run", "--adapter", "generic-process", "--execution-id", executionID.String(), "--", script})
	}()

	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		var statusOut, statusErr bytes.Buffer
		observer := testApp(&statusOut, &statusErr)
		if code := observer.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "status", executionID.String()}); code == 0 {
			var statusDoc struct {
				Result model.Execution `json:"result"`
			}
			if json.Unmarshal(statusOut.Bytes(), &statusDoc) == nil && statusDoc.Result.State == model.StateRunning && statusDoc.Result.Liveness == model.LivenessAlive && statusDoc.Result.Observation.FreshForSeconds != nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution was not observable while running; run_output=%s", runOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	var subscribeOut, subscribeErr bytes.Buffer
	subscriber := testApp(&subscribeOut, &subscribeErr)
	callbackPath := filepath.Join(root, "callbacks", "events.ndjson")
	started := time.Now()
	if code := subscriber.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "subscribe", "create", "--execution", executionID.String(), "--kind", "terminal", "--destination", "file", "--target", callbackPath}); code != 0 {
		t.Fatalf("subscribe while running exit=%d output=%s", code, subscribeOut.String())
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("subscribe blocked on run journal lock for %s", elapsed)
	}

	var supervisorOut, supervisorErr bytes.Buffer
	supervisorApp := testApp(&supervisorOut, &supervisorErr)
	if code := supervisorApp.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "supervisor", "run", "--once", "--state-dir", filepath.Join(root, "supervisor")}); code != 0 {
		t.Fatalf("supervisor cycle exit=%d output=%s stderr=%s", code, supervisorOut.String(), supervisorErr.String())
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	live, err := journal.GetExecution(context.Background(), executionID)
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	if live.State != model.StateRunning || live.Liveness != model.LivenessAlive || live.Observation.Source != model.ObservationNativeStream || live.Observation.Integrity != model.IntegrityVerified || live.Observation.FreshForSeconds == nil {
		t.Fatalf("supervisor corrupted active runner lease: %#v", live)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run exit=%d output=%s stderr=%s", code, runOut.String(), runErr.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatal("run did not terminate")
	}
}

func TestIncompleteMutationReturnsStableUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"subscribe", "--output", "json"})
	if code != 2 {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"code":"usage"`) {
		t.Fatalf("output=%s", stdout.String())
	}
}

func TestSubscriptionAndPromotionPlanUseTypedDurableState(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "state", "journal.db")
	configPath := filepath.Join(root, "config", "config.json")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	native := `{"type":"result","status":"completed","result":{"summary":"done"}}`
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--adapter", "generic-process", "--", "/bin/echo", native}); code != 0 {
		t.Fatalf("run exit=%d output=%s", code, stdout.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	eventPath := filepath.Join(root, "callbacks", "events.ndjson")
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "subscribe", "create", "--execution", runDoc.Result.ID.String(), "--kind", "terminal", "--destination", "file", "--target", eventPath}); code != 0 {
		t.Fatalf("subscribe exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"subscription"`) || !strings.Contains(stdout.String(), `"sub-`) {
		t.Fatalf("subscription output=%s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--config", configPath, "config", "set-profile", "--name", "fleet", "--multica-executable", "/bin/echo", "--multica-profile", "desktop", "--workspace-id", "workspace-test", "--server-url", "https://multica.example.test", "--app-url", "https://multica.example.test", "--default"}); code != 0 {
		t.Fatalf("config exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "--config", configPath, "--profile", "fleet", "promote", runDoc.Result.ID.String(), "--title", "Continue durable work", "--plan"}); code != 0 {
		t.Fatalf("promote plan exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"side_effect_class":"remote_coordination_write"`) || !strings.Contains(stdout.String(), `"client_key":"sha256:`) {
		t.Fatalf("promotion plan output=%s", stdout.String())
	}
}

func TestPromotionExecuteReplayReturnsPersistedAliasAndOneLifecycle(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "state", "journal.db")
	configPath := filepath.Join(root, "config", "config.json")
	fakeMultica := filepath.Join(root, "fake-multica")
	if err := os.WriteFile(fakeMultica, []byte("#!/bin/sh\nprintf '%s\\n' '{\"id\":\"opaque-multica-uuid\",\"identifier\":\"SCA-777\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	native := `{"type":"result","status":"completed","result":{"summary":"done"}}`
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", native}); code != 0 {
		t.Fatalf("run exit=%d output=%s", code, stdout.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--config", configPath, "config", "set-profile", "--name", "fleet", "--multica-executable", fakeMultica, "--multica-profile", "desktop", "--workspace-id", "workspace-test", "--server-url", "https://multica.example.test", "--app-url", "https://multica.example.test", "--default"}); code != 0 {
		t.Fatalf("config exit=%d output=%s", code, stdout.String())
	}
	type promotionResult struct {
		Execution  model.Execution `json:"execution"`
		IssueAlias string          `json:"issue_alias"`
		Reused     bool            `json:"reused"`
	}
	var first, second struct {
		Result promotionResult `json:"result"`
	}
	args := []string{"--output", "json", "--journal", journalPath, "--config", configPath, "--profile", "fleet", "promote", runDoc.Result.ID.String(), "--title", "Continue durable work"}
	stdout.Reset()
	if code := a.run(context.Background(), args); code != 0 {
		t.Fatalf("first promote exit=%d output=%s", code, stdout.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), args); code != 0 {
		t.Fatalf("replay promote exit=%d output=%s", code, stdout.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if first.Result.Execution.ID != second.Result.Execution.ID || first.Result.IssueAlias != second.Result.IssueAlias {
		t.Fatalf("replay changed durable aliases: first=%#v second=%#v", first.Result, second.Result)
	}
	if first.Result.Reused || !second.Result.Reused {
		t.Fatalf("unexpected replay flags: first=%v second=%v", first.Result.Reused, second.Result.Reused)
	}
	foundDurableEvents := false
	for _, capability := range first.Result.Execution.Capabilities.Items {
		if capability.Name == "events" {
			foundDurableEvents = capability.Constraints["cross_restart"] == true && capability.Constraints["scope"] == "workspace_events"
		}
	}
	if !foundDurableEvents {
		t.Fatalf("promotion omitted cross-restart durable event capability: %#v", first.Result.Execution.Capabilities)
	}
	if err := os.WriteFile(fakeMultica, []byte("#!/bin/sh\nprintf '%s\\n' 'The request conflicts with the current state of the resource.' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	changedArgs := append([]string(nil), args...)
	changedArgs[len(changedArgs)-1] = "Changed durable work"
	if code := a.run(context.Background(), changedArgs); code != 8 || !strings.Contains(stdout.String(), `"code":"conflict"`) {
		t.Fatalf("changed promotion exit=%d output=%s", code, stdout.String())
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	executions, err := journal.ListExecutions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(executions) != 2 {
		t.Fatalf("promotion created %d total lifecycles; want source plus one target", len(executions))
	}
}
