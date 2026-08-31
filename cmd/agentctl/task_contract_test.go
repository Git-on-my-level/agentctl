package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func writeTaskContractFixture(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "task-contract.json")
	content := `{
  "objective_summary": "Diagnose the failing service",
  "side_effect_boundary": "read_only",
  "expected_artifact_kinds": ["root_cause_report"],
  "continuation": {"same_session_required": true}
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunTaskContractRetainedByStatusAndResult(t *testing.T) {
	root := t.TempDir()
	contractPath := writeTaskContractFixture(t, root)
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	argv := []string{"--journal", journal, "run", "--adapter", "generic-process", "--task-contract", contractPath, "--", "/bin/echo", `{"type":"result","status":"completed","result":"CONTRACT_RESULT_OK"}`}
	if code := a.run(context.Background(), argv); code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var runDoc struct {
		Result   model.Execution  `json:"result"`
		Warnings []output.Warning `json:"warnings"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	if runDoc.Result.TaskContract == nil || runDoc.Result.TaskContract.ObjectiveSummary != "Diagnose the failing service" || !reflect.DeepEqual(runDoc.Result.TaskContract.ExpectedArtifactKinds, []string{"root_cause_report"}) {
		t.Fatalf("run task contract=%#v", runDoc.Result.TaskContract)
	}
	if len(runDoc.Warnings) == 0 || runDoc.Warnings[0].Code != "acceptance_external_required" {
		t.Fatalf("run warnings=%#v", runDoc.Warnings)
	}

	for _, command := range [][]string{{"status", runDoc.Result.ID.String()}, {"result", runDoc.Result.ID.String()}} {
		stdout.Reset()
		args := append([]string{"--journal", journal}, command...)
		if code := a.run(context.Background(), args); code != 0 {
			t.Fatalf("%s exit=%d output=%s", command[0], code, stdout.String())
		}
		if !strings.Contains(stdout.String(), `"task_contract":{"objective_summary":"Diagnose the failing service"`) || !strings.Contains(stdout.String(), `"code":"acceptance_external_required"`) {
			t.Fatalf("%s omitted contract or acceptance boundary: %s", command[0], stdout.String())
		}
	}
}

func TestRunTaskContractPlanValidatesWithoutJournalWrite(t *testing.T) {
	root := t.TempDir()
	contractPath := writeTaskContractFixture(t, root)
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journal, "run", "--plan", "--adapter", "generic-process", "--task-contract", contractPath, "--", "/bin/echo", "done"}); code != 0 {
		t.Fatalf("plan exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"task_contract"`) || !strings.Contains(stdout.String(), `"acceptance":"external_required"`) || !strings.Contains(stdout.String(), `"task_contract_digest":"sha256:`) {
		t.Fatalf("plan omitted typed contract: %s", stdout.String())
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("plan created journal: %v", err)
	}
}

func TestRunTaskContractInputFailsClosed(t *testing.T) {
	root := t.TempDir()
	unknown := filepath.Join(root, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"objective_summary":"bounded","surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(root, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"expected_artifact_kinds":["Not Typed"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--task-contract", unknown, "--", "/bin/echo", "done"},
		{"--task-contract", invalid, "--", "/bin/echo", "done"},
		{"--task-contract", "-", "--", "/bin/echo", "done"},
	} {
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		if code := a.run(context.Background(), append([]string{"run"}, args...)); code != output.ExitCodeFor(output.CodeUsage) {
			t.Fatalf("args=%v exit=%d output=%s", args, code, stdout.String())
		}
	}
}

func TestRunTaskContractRejectsMulticaAuthority(t *testing.T) {
	root := t.TempDir()
	contractPath := writeTaskContractFixture(t, root)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"run", "--adapter", "multica", "--task-contract", contractPath, "--", "/bin/echo", "done"}); code != output.ExitCodeFor(output.CodeCapabilityUnavailable) {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "Multica issues remain authoritative") {
		t.Fatalf("missing authority explanation: %s", stdout.String())
	}
}

func TestBackgroundCommandPreservesTaskContractAndPinsDigest(t *testing.T) {
	root := t.TempDir()
	contractPath := writeTaskContractFixture(t, root)
	payload, problem := loadTaskContract(contractPath)
	if problem != nil {
		t.Fatal(problem)
	}
	args := backgroundCommandArgs(common{}, []string{"--background", "--task-contract", contractPath, "--", "/bin/echo", "done"}, "exec-amber-willow-orbit-tiger-harbor-gentle", true, false)
	want := []string{"--output", "json", "run", "--task-contract", contractPath, "--execution-id", "exec-amber-willow-orbit-tiger-harbor-gentle", "--", "/bin/echo", "done"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v want=%v", args, want)
	}
	t.Setenv(backgroundReadyTokenEnv, "ready")
	t.Setenv(backgroundTaskContractDigestEnv, payload.Digest)
	if got := backgroundTaskContractDigest(); got != payload.Digest {
		t.Fatalf("digest=%q want=%q", got, payload.Digest)
	}
	if got := os.Getenv(backgroundTaskContractDigestEnv); got != "" {
		t.Fatalf("internal digest leaked beyond worker validation: %q", got)
	}
}

func TestBackgroundRunRetainsTaskContractThroughBuiltBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generic shell fixture is Unix-only")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "agentctl")
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".")
	if outputBytes, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture binary: %v\n%s", err, outputBytes)
	}
	contractPath := writeTaskContractFixture(t, root)
	journal := filepath.Join(root, "state", "journal.db")
	native := `sleep 1; printf '%s\n' '{"type":"result","status":"completed","result":"BACKGROUND_CONTRACT_OK"}'`
	launch := exec.Command(binary, "--journal", journal, "run", "--background", "--task-contract", contractPath, "--timeout", "30s", "--adapter", "generic-process", "--", "/bin/sh", "-c", native)
	launchOutput, err := launch.CombinedOutput()
	if err != nil {
		t.Fatalf("background launch: %v\n%s", err, launchOutput)
	}
	var launchDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(launchOutput, &launchDoc); err != nil {
		t.Fatal(err)
	}
	if launchDoc.Result.TaskContract == nil || launchDoc.Result.TaskContract.ObjectiveSummary != "Diagnose the failing service" {
		t.Fatalf("background launch omitted task contract: %s", launchOutput)
	}
	wait := exec.Command(binary, "--journal", journal, "await", launchDoc.Result.ID.String(), "--no-timeout", "--ignore-attention")
	if waitOutput, err := wait.CombinedOutput(); err != nil {
		t.Fatalf("await: %v\n%s", err, waitOutput)
	}
	result := exec.Command(binary, "--journal", journal, "result", launchDoc.Result.ID.String())
	resultOutput, err := result.CombinedOutput()
	if err != nil || !bytes.Contains(resultOutput, []byte("BACKGROUND_CONTRACT_OK")) || !bytes.Contains(resultOutput, []byte(`"task_contract"`)) || !bytes.Contains(resultOutput, []byte(`"code":"acceptance_external_required"`)) {
		t.Fatalf("background result: %v\n%s", err, resultOutput)
	}
}

func TestExecutionSchemaPublishesCompleteTaskContractShape(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(schemaRepositoryRoot(t), "schemas", "execution.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("execution schema has no $defs")
	}
	taskContract, ok := defs["task_contract"].(map[string]any)
	if !ok {
		t.Fatal("execution schema has no task_contract definition")
	}
	properties, ok := taskContract["properties"].(map[string]any)
	if !ok {
		t.Fatal("task_contract schema has no properties")
	}
	for _, field := range []string{"objective_summary", "side_effect_boundary", "acceptance_ref", "expected_artifact_kinds", "continuation", "provenance"} {
		if _, ok := properties[field]; !ok {
			t.Errorf("task_contract schema omitted %q", field)
		}
	}
}
