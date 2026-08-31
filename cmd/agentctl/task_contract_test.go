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

	"github.com/Git-on-my-level/agentctl/internal/ids"
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
	empty := filepath.Join(root, "empty.json")
	if err := os.WriteFile(empty, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyArtifacts := filepath.Join(root, "empty-artifacts.json")
	if err := os.WriteFile(emptyArtifacts, []byte(`{"expected_artifact_kinds":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	nullObjective := filepath.Join(root, "null-objective.json")
	if err := os.WriteFile(nullObjective, []byte(`{"objective_summary":null,"side_effect_boundary":"read_only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyContinuation := filepath.Join(root, "empty-continuation.json")
	if err := os.WriteFile(emptyContinuation, []byte(`{"continuation":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--task-contract", unknown, "--", "/bin/echo", "done"},
		{"--task-contract", invalid, "--", "/bin/echo", "done"},
		{"--task-contract", empty, "--", "/bin/echo", "done"},
		{"--task-contract", emptyArtifacts, "--", "/bin/echo", "done"},
		{"--task-contract", nullObjective, "--", "/bin/echo", "done"},
		{"--task-contract", emptyContinuation, "--", "/bin/echo", "done"},
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

func TestTaskContractInputSchemaIsStrictWhileExecutionV1IsTolerant(t *testing.T) {
	root := schemaRepositoryRoot(t)
	readSchema := func(t *testing.T, name string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, "schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		return schema
	}
	executionSchema := readSchema(t, "execution.schema.json")
	stored := executionSchema["$defs"].(map[string]any)["task_contract"].(map[string]any)
	if stored["additionalProperties"] != true {
		t.Fatalf("stored execution-v1 task contract is not extension-tolerant: %#v", stored)
	}
	storedArtifacts := stored["properties"].(map[string]any)["expected_artifact_kinds"].(map[string]any)
	if _, tightened := storedArtifacts["minItems"]; tightened {
		t.Fatalf("stored execution-v1 rejects legacy empty artifact arrays: %#v", storedArtifacts)
	}
	input := readSchema(t, "task-contract-input.schema.json")
	if input["additionalProperties"] != false {
		t.Fatalf("new task contract input is not strict: %#v", input)
	}
	inputArtifacts := input["properties"].(map[string]any)["expected_artifact_kinds"].(map[string]any)
	if inputArtifacts["minItems"] != float64(1) {
		t.Fatalf("task contract input does not reject empty artifact arrays: %#v", inputArtifacts)
	}
}

func TestPromotedTaskContractWarningsAreAuthorityNeutral(t *testing.T) {
	executionID, err := ids.NewExecutionID(ids.CryptoGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := ids.NewHostID(ids.CryptoGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	execution := model.Execution{
		ID:           executionID,
		OriginHostID: hostID,
		Authority:    model.AuthorityMultica,
		Adapter:      "multica",
		Mode:         model.ModeMultica,
		Acquisition:  model.AcquisitionPromoted,
		State:        model.StateWaiting,
		TaskContract: &model.TaskContract{ObjectiveSummary: "Continue durable work", SideEffectBoundary: "multica_issue"},
	}
	assertWarning := func(t *testing.T, raw []byte) {
		t.Helper()
		var document struct {
			Warnings []output.Warning `json:"warnings"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		if len(document.Warnings) != 1 || document.Warnings[0].Code != "acceptance_external_required" {
			t.Fatalf("warnings=%#v", document.Warnings)
		}
		message := document.Warnings[0].Message
		if strings.Contains(strings.ToLower(message), "native") || message != "the execution state does not prove the task contract's expected artifacts or acceptance; verify them through their named authority" {
			t.Fatalf("warning is not authority-neutral: %q", message)
		}
	}

	t.Run("status", func(t *testing.T) {
		var stdout bytes.Buffer
		if problem := writeExecution(output.Renderer{Mode: output.JSON, Writer: &stdout}, execution, "status"); problem != nil {
			t.Fatal(problem)
		}
		assertWarning(t, stdout.Bytes())
	})

	t.Run("result", func(t *testing.T) {
		terminal := execution
		terminal.State = model.StateCompleted
		var stdout bytes.Buffer
		outcome := model.Outcome{SchemaVersion: model.SchemaVersion, ExecutionID: terminal.ID, State: model.StateCompleted, Availability: model.OutcomeStored}
		if problem := writeExecutionOutcome(output.Renderer{Mode: output.JSON, Writer: &stdout}, terminal, outcome); problem != nil {
			t.Fatal(problem)
		}
		assertWarning(t, stdout.Bytes())
	})
}
