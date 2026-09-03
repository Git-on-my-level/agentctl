package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("delivery failed") }

func testApp(stdout, stderr *bytes.Buffer) *app {
	return &app{stdout: stdout, stderr: stderr, getenv: func(string) string { return "" }, now: time.Now}
}

func TestAutomaticMaintenanceRunsOnlyWithExternalWork(t *testing.T) {
	warning := &output.Warning{Code: "agentctl_update_available", Message: "update available", Details: map[string]any{"current_version": "v0.3.2", "latest_version": "v0.3.3"}}
	for _, test := range []struct {
		args       []string
		wantCode   int
		wantCalled bool
	}{
		{args: []string{"version"}, wantCode: 0},
		{args: []string{"unknown-command"}, wantCode: 2},
		{args: []string{"run", "--plan", "--allow-missing-result", "--", "/bin/true"}, wantCode: -1},
		{args: []string{"run", "--allow-missing-result", "--", "/bin/true"}, wantCode: -1, wantCalled: true},
		{args: []string{"dispatch", "--plan"}, wantCode: -1},
		{args: []string{"dispatch"}, wantCode: -1, wantCalled: true},
	} {
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		called := false
		a.updateNotice = func(context.Context, string, common) *output.Warning { called = true; return warning }
		if code := a.run(context.Background(), test.args); test.wantCode >= 0 && code != test.wantCode {
			t.Fatalf("args=%v exit=%d output=%s", test.args, code, stdout.String())
		}
		if called != test.wantCalled {
			t.Fatalf("args=%v update hook called=%v want=%v", test.args, called, test.wantCalled)
		}
		if test.wantCalled && (!strings.Contains(stdout.String(), `"warnings":[{"code":"agentctl_update_available"`) || !strings.Contains(stdout.String(), `"latest_version":"v0.3.3"`)) {
			t.Fatalf("args=%v missing update notice: %s", test.args, stdout.String())
		}
		if !test.wantCalled && strings.Contains(stdout.String(), `agentctl_update_available`) {
			t.Fatalf("args=%v read-only command included update notice: %s", test.args, stdout.String())
		}
	}
}

func TestAutomaticMaintenanceSkipsGlobalParseErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	called := false
	a.updateNotice = func(context.Context, string, common) *output.Warning {
		called = true
		return &output.Warning{Code: "agentctl_update_available", Message: "update available"}
	}
	if code := a.run(context.Background(), []string{"--output", "invalid"}); code != 2 {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"code":"usage"`) || strings.Contains(stdout.String(), `agentctl_update_available`) || called {
		t.Fatalf("parse error ran automatic maintenance: called=%v output=%s", called, stdout.String())
	}
}

func TestSkillsCommandsDoNotStartInvocationUpdateWorker(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	called := 0
	a.updateNotice = func(context.Context, string, common) *output.Warning {
		called++
		return nil
	}
	if code := a.run(context.Background(), []string{"skills"}); code != 2 {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	if called != 0 {
		t.Fatalf("skills command ran invocation update preflight %d times", called)
	}
}

func TestUpdateCheckStatePathIsGlobalAcrossSelectedJournals(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	custom := filepath.Join(t.TempDir(), "isolated", "journal.db")
	path, err := updateCheckStatePath(common{journalPath: custom})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(stateRoot, "agentctl", "update-state.json"); path != want {
		t.Fatalf("path=%s want=%s", path, want)
	}
}

func TestUpdatePolicyAndStatusCommandsUseOwnerGlobalState(t *testing.T) {
	root := t.TempDir()
	getenv := func(name string) string {
		if name == "XDG_STATE_HOME" {
			return root
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.getenv = getenv
	if code := a.run(context.Background(), []string{"update", "policy", "notify"}); code != 0 {
		t.Fatalf("policy exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"update", "status"}); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"mode":"notify"`) || !strings.Contains(stdout.String(), filepath.Join(root, "agentctl", "update-state.json")) {
		t.Fatalf("unexpected status: %s", stdout.String())
	}
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
		got := mapAdapterError("adapter probe failed", err)
		if got.Code != test.want || got.Details["reason"] != "probe fixture" {
			t.Fatalf("adapter code %s mapped to %#v; want %s with reason", test.adapterCode, got, test.want)
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

func TestEventsRejectsLimitOutsideNativeIntRangeBeforeCreatingJournal(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal.db")
	executionID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journal, "events", executionID.String(), "--limit", "18446744073709551615"}); code != 2 {
		t.Fatalf("events exit=%d output=%s", code, stdout.String())
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("invalid pagination created journal: %v", err)
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
	if !strings.Contains(stdout.String(), `"bootstrap"`) || !strings.Contains(stdout.String(), `"journal"`) || !strings.Contains(stdout.String(), `"adapters"`) || !strings.Contains(stdout.String(), `"generic-process"`) {
		t.Fatalf("doctor omitted readiness dimensions: %s", stdout.String())
	}
}

func TestDoctorIncludesDetectedNativeAdaptersAlongsideConfiguredAuthority(t *testing.T) {
	got := doctorAdapterSelection(nil, []string{"multica"}, []string{"claude", "codex", "cursor", "hermes"})
	want := []string{"claude", "codex", "cursor", "multica"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selection=%v, want %v", got, want)
	}
	if explicit := doctorAdapterSelection([]string{"cursor"}, []string{"multica"}, []string{"codex"}); !reflect.DeepEqual(explicit, []string{"cursor"}) {
		t.Fatalf("explicit selection widened unexpectedly: %v", explicit)
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
	if classes["attach"] != "read_only" || classes["recent"] != "read_only" || classes["inbox"] != "read_only" || classes["result"] != "local_operational_write" || classes["await"] != "local_operational_write" || classes["context"] != "local_operational_write" || classes["data"] != "local_operational_write" {
		t.Fatalf("unexpected side-effect classes: %#v", classes)
	}
}

func TestDefaultOutputIsJSONAndTopicHelpIsProgressive(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"help", "bootstrap", "update"}); code != 0 {
		t.Fatalf("help exit=%d output=%s", code, stdout.String())
	}
	var doc struct {
		OK     bool        `json:"ok"`
		Result commandHelp `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.OK || doc.Result.Name != "bootstrap update" || !strings.Contains(doc.Result.Usage, "--dry-run") {
		t.Fatalf("topic help=%#v", doc)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"run", "--help"}); code != 0 || !strings.Contains(stdout.String(), `"name":"run"`) {
		t.Fatalf("inline help exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"help", "missing-topic"}); code != 3 || !strings.Contains(stdout.String(), `"code":"not_found"`) {
		t.Fatalf("unknown help exit=%d output=%s", code, stdout.String())
	}
}

func TestRunDefaultsInferAdapterWithoutWallTimeout(t *testing.T) {
	tests := []struct{ executable, adapter string }{
		{"/opt/bin/codex", "codex"}, {"cursor-agent", "cursor"}, {"claude.exe", "claude"}, {"omp", "omp"}, {"/bin/echo", "generic-process"},
	}
	for _, test := range tests {
		opts, problem := parseRun([]string{"--", test.executable, "task"})
		if problem != nil {
			t.Fatalf("parse %s: %v", test.executable, problem)
		}
		if opts.adapter != test.adapter || opts.timeout != 0 {
			t.Fatalf("parse %s adapter=%s timeout=%s", test.executable, opts.adapter, opts.timeout)
		}
	}
	opts, problem := parseRun([]string{"--adapter", "generic-process", "--no-timeout", "--", "codex", "task"})
	if problem != nil || opts.adapter != "generic-process" || opts.timeout != 0 {
		t.Fatalf("explicit override=%#v problem=%v", opts, problem)
	}
}

func TestForegroundRunEnvelopeTeachesOwnership(t *testing.T) {
	executionID, err := ids.NewExecutionID(ids.CryptoGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := ids.NewHostID(ids.CryptoGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	execution := model.Execution{ID: executionID, OriginHostID: hostID, Authority: model.AuthorityNative, Adapter: "codex", State: model.StateCompleted}
	var stdout bytes.Buffer
	if problem := writeExecution(output.Renderer{Mode: output.JSON, Writer: &stdout}, execution, "run"); problem != nil {
		t.Fatal(problem)
	}
	if !strings.Contains(stdout.String(), `"code":"foreground_execution_owned"`) || !strings.Contains(stdout.String(), "no default wall-clock timeout") || !strings.Contains(stdout.String(), `"argv":["agentctl","help","run"]`) {
		t.Fatalf("foreground run envelope omitted ownership guidance: %s", stdout.String())
	}
	stdout.Reset()
	if problem := writeExecution(output.Renderer{Mode: output.JSON, Writer: &stdout}, execution, "background"); problem != nil {
		t.Fatal(problem)
	}
	if strings.Contains(stdout.String(), "foreground_execution_owned") {
		t.Fatalf("background envelope received foreground warning: %s", stdout.String())
	}
}

func TestWriteExecutionAwaitNextActionMatchesDeadline(t *testing.T) {
	executionID, err := ids.NewExecutionID(ids.CryptoGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := ids.NewHostID(ids.CryptoGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	base := model.Execution{ID: executionID, OriginHostID: hostID, Authority: model.AuthorityNative, Adapter: "codex", State: model.StateRunning}
	awaitArgv := func(t *testing.T, execution model.Execution, operation string) (label string, argv []string) {
		t.Helper()
		var stdout bytes.Buffer
		if problem := writeExecution(output.Renderer{Mode: output.JSON, Writer: &stdout}, execution, operation); problem != nil {
			t.Fatal(problem)
		}
		var document struct {
			NextActions []output.NextAction `json:"next_actions"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		for _, action := range document.NextActions {
			if len(action.Argv) >= 2 && action.Argv[1] == "await" {
				return action.Label, action.Argv
			}
		}
		t.Fatalf("missing await next_action: %s", stdout.String())
		return "", nil
	}

	label, argv := awaitArgv(t, base, "background")
	wantUnbounded := []string{"agentctl", "await", executionID.String(), "--output", "json", "--no-timeout"}
	if label == "" || strings.Contains(strings.ToLower(label), "10 minutes") || strings.Contains(strings.ToLower(label), "ten-minute") {
		t.Fatalf("unbounded await label=%q", label)
	}
	if !reflect.DeepEqual(argv, wantUnbounded) {
		t.Fatalf("unbounded await argv=%q want=%q", argv, wantUnbounded)
	}

	deadline := time.Now().UTC().Add(30 * time.Second)
	bounded := base
	bounded.DeadlineAt = &deadline
	label, argv = awaitArgv(t, bounded, "background")
	wantBounded := []string{"agentctl", "await", executionID.String(), "--output", "json", "--through-execution-deadline"}
	if label != "Wait through execution deadline" {
		t.Fatalf("bounded await label=%q", label)
	}
	if !reflect.DeepEqual(argv, wantBounded) {
		t.Fatalf("bounded await argv=%q want=%q", argv, wantBounded)
	}
}

func TestRunParsesLabelsAndBackground(t *testing.T) {
	opts, problem := parseRun([]string{"--background", "--label", "review", "--label", "model.grok", "--", "/bin/echo", "done"})
	if problem != nil {
		t.Fatal(problem)
	}
	if !opts.background || !reflect.DeepEqual(opts.labels, []string{"review", "model.grok"}) {
		t.Fatalf("opts=%#v", opts)
	}
	for _, args := range [][]string{
		{"--label", "Not-Lowercase", "--", "/bin/echo"},
		{"--label", "review", "--label", "review", "--", "/bin/echo"},
		{"--background", "--idempotency-key", "same", "--", "/bin/echo"},
	} {
		if _, problem := parseRun(args); problem == nil || problem.Code != output.CodeUsage {
			t.Fatalf("args=%v problem=%#v", args, problem)
		}
	}
}

func TestBackgroundCommandArgsPreserveSelectorsAndRemoveBackground(t *testing.T) {
	executionID, err := ids.NewExecutionID(ids.CryptoGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	args := backgroundCommandArgs(common{profile: "fleet", journalPath: "/tmp/journal.db"}, []string{"--background", "--label", "review", "--", "/bin/echo", "done"}, executionID, true, false)
	want := []string{"--output", "json", "--profile", "fleet", "--journal", "/tmp/journal.db", "run", "--label", "review", "--execution-id", executionID.String(), "--", "/bin/echo", "done"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v want=%v", args, want)
	}
	args = backgroundCommandArgs(common{}, []string{"--background", "--", "/bin/echo", "--background"}, executionID, true, false)
	want = []string{"--output", "json", "run", "--execution-id", executionID.String(), "--", "/bin/echo", "--background"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("native argv was rewritten: args=%v want=%v", args, want)
	}
	args = backgroundCommandArgs(common{}, []string{"--background", "--prompt-file", "task.md", "--prompt-delivery", "stdin", "--", "/bin/cat"}, executionID, true, true)
	want = []string{"--output", "json", "run", "--prompt-stdin", "--prompt-delivery", "stdin", "--execution-id", executionID.String(), "--", "/bin/cat"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("materialized prompt args=%v want=%v", args, want)
	}
}

func TestReadBackgroundExecutionRetriesJournalContention(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "journal.db")
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{Authority: model.AuthorityNative, Adapter: "generic-process", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}}, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = journal.Close()
	}()
	read, found, problem := readBackgroundExecution(context.Background(), journalPath, execution.ID)
	if problem != nil || !found || read.ID != execution.ID {
		t.Fatalf("read=%#v found=%v problem=%#v", read, found, problem)
	}
}

func TestBackgroundRunLifecycleThroughBuiltBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generic shell fixture is Unix-only")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "agentctl")
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".")
	buildOutput, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture binary: %v\n%s", err, buildOutput)
	}
	rejectedJournal := filepath.Join(root, "rejected", "journal.db")
	rejected := exec.Command(binary, "--journal", rejectedJournal, "run", "--background", "--adapter", "codex", "--", "/bin/echo", "exec", "review")
	rejectedOutput, rejectedErr := rejected.CombinedOutput()
	if rejectedErr == nil || !bytes.Contains(rejectedOutput, []byte(`"code":"capability_unavailable"`)) || !bytes.Contains(rejectedOutput, []byte(`"diagnostic_code":"invocation_output_mode_required"`)) {
		t.Fatalf("background worker accepted invocation without structured output: %v\n%s", rejectedErr, rejectedOutput)
	}
	if _, err := os.Stat(rejectedJournal); !os.IsNotExist(err) {
		t.Fatalf("background preflight rejection created a journal: %v", err)
	}
	journal := filepath.Join(root, "state", "journal.db")
	native := `sleep 2; printf '%s\n' '{"type":"result","status":"completed","result":"BACKGROUND_INTEGRATION_OK"}'`
	started := time.Now()
	launch := exec.Command(binary, "--journal", journal, "run", "--background", "--label", "integration", "--timeout", "30s", "--adapter", "generic-process", "--", "/bin/sh", "-c", native)
	launchOutput, err := launch.CombinedOutput()
	if err != nil {
		t.Fatalf("background launch: %v\n%s", err, launchOutput)
	}
	if elapsed := time.Since(started); elapsed >= 1500*time.Millisecond {
		t.Fatalf("background launch waited for native completion: %s", elapsed)
	}
	var launchDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(launchOutput, &launchDoc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launchDoc.Result.Labels, []string{"integration"}) || launchDoc.Result.State.Terminal() || launchDoc.Result.DeadlineAt == nil {
		t.Fatalf("launch=%#v", launchDoc.Result)
	}
	if !bytes.Contains(launchOutput, []byte(`"--through-execution-deadline"`)) || !bytes.Contains(launchOutput, []byte(`"agentctl","help","subscribe"`)) {
		t.Fatalf("background next actions are not supervision-aware: %s", launchOutput)
	}
	recent := exec.Command(binary, "--journal", journal, "recent", "--label", "integration", "--limit", "1")
	recentOutput, err := recent.CombinedOutput()
	if err != nil || !bytes.Contains(recentOutput, []byte(launchDoc.Result.ID.String())) {
		t.Fatalf("recent: %v\n%s", err, recentOutput)
	}
	wait := exec.Command(binary, "--journal", journal, "await", launchDoc.Result.ID.String(), "--no-timeout", "--ignore-attention")
	if waitOutput, err := wait.CombinedOutput(); err != nil {
		t.Fatalf("await: %v\n%s", err, waitOutput)
	}
	result := exec.Command(binary, "--journal", journal, "result", launchDoc.Result.ID.String(), "--min-result-bytes", "10")
	resultOutput, err := result.CombinedOutput()
	if err != nil || !bytes.Contains(resultOutput, []byte("BACKGROUND_INTEGRATION_OK")) {
		t.Fatalf("result: %v\n%s", err, resultOutput)
	}
	promptNative := `IFS= read -r value; printf '{"type":"result","status":"completed","result":"%s"}\n' "$value"`
	promptLaunch := exec.Command(binary, "--journal", journal, "run", "--background", "--label", "prompt-stdin", "--prompt-stdin", "--prompt-delivery", "stdin", "--adapter", "generic-process", "--", "/bin/sh", "-c", promptNative)
	promptLaunch.Stdin = strings.NewReader("BACKGROUND_STDIN_OK\n")
	promptLaunchOutput, err := promptLaunch.CombinedOutput()
	if err != nil {
		t.Fatalf("background prompt stdin launch: %v\n%s", err, promptLaunchOutput)
	}
	var promptLaunchDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(promptLaunchOutput, &promptLaunchDoc); err != nil {
		t.Fatal(err)
	}
	promptWait := exec.Command(binary, "--journal", journal, "await", promptLaunchDoc.Result.ID.String(), "--no-timeout", "--ignore-attention")
	if promptWaitOutput, err := promptWait.CombinedOutput(); err != nil {
		t.Fatalf("background prompt stdin await: %v\n%s", err, promptWaitOutput)
	}
	promptResult := exec.Command(binary, "--journal", journal, "result", promptLaunchDoc.Result.ID.String(), "--min-result-bytes", "10")
	if promptResultOutput, err := promptResult.CombinedOutput(); err != nil || !bytes.Contains(promptResultOutput, []byte("BACKGROUND_STDIN_OK")) {
		t.Fatalf("background prompt stdin result: %v\n%s", err, promptResultOutput)
	}
	list := exec.Command(binary, "--journal", journal, "list", "--label", "prompt-stdin", "--limit", "1")
	if listOutput, err := list.CombinedOutput(); err != nil || !bytes.Contains(listOutput, []byte(promptLaunchDoc.Result.ID.String())) {
		t.Fatalf("list alias: %v\n%s", err, listOutput)
	}
	promptFile := filepath.Join(root, "background-task.md")
	if err := os.WriteFile(promptFile, []byte("PROMPT_FILE_SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	promptFileNative := `IFS= read -r value; test "$value" = "PROMPT_FILE_SECRET"; printf '%s\n' '{"type":"result","status":"completed","result":"BACKGROUND_FILE_OK"}'`
	promptFileLaunch := exec.Command(binary, "--journal", journal, "run", "--background", "--cwd", root, "--label", "prompt-file", "--prompt-file", "background-task.md", "--prompt-delivery", "stdin", "--adapter", "generic-process", "--", "/bin/sh", "-c", promptFileNative)
	promptFileLaunchOutput, err := promptFileLaunch.CombinedOutput()
	if err != nil {
		t.Fatalf("background prompt file launch: %v\n%s", err, promptFileLaunchOutput)
	}
	var promptFileLaunchDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(promptFileLaunchOutput, &promptFileLaunchDoc); err != nil {
		t.Fatal(err)
	}
	promptFileWait := exec.Command(binary, "--journal", journal, "await", promptFileLaunchDoc.Result.ID.String(), "--no-timeout", "--ignore-attention")
	if promptFileWaitOutput, err := promptFileWait.CombinedOutput(); err != nil {
		t.Fatalf("background prompt file await: %v\n%s", err, promptFileWaitOutput)
	}
	promptFileResult := exec.Command(binary, "--journal", journal, "result", promptFileLaunchDoc.Result.ID.String(), "--min-result-bytes", "10")
	if promptFileResultOutput, err := promptFileResult.CombinedOutput(); err != nil || !bytes.Contains(promptFileResultOutput, []byte("BACKGROUND_FILE_OK")) {
		t.Fatalf("background prompt file result: %v\n%s", err, promptFileResultOutput)
	}
	journalBytes, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journalBytes, []byte("PROMPT_FILE_SECRET")) || bytes.Contains(journalBytes, []byte(promptFile)) {
		t.Fatal("background prompt source leaked into durable journal")
	}
	ompFixture := filepath.Join(root, "omp-fixture")
	ompContents := `#!/bin/sh
set -eu
if [ "${1:-}" = "--version" ]; then
  printf '%s\n' 'omp/17.3.5'
  exit 0
fi
sleep 1
printf '%s\n' '{"type":"session","version":3,"id":"omp-background-fixture"}' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"OMP_BACKGROUND_RESULT_OK"}],"stopReason":"stop"}}' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"OMP_BACKGROUND_RESULT_OK"}],"stopReason":"stop"}]}'
`
	if err := os.WriteFile(ompFixture, []byte(ompContents), 0o700); err != nil {
		t.Fatal(err)
	}
	ompLaunch := exec.Command(binary, "--journal", journal, "run", "--background", "--label", "omp-integration", "--adapter", "omp", "--", ompFixture, "-p", "--mode", "json", "probe")
	ompLaunchOutput, err := ompLaunch.CombinedOutput()
	if err != nil {
		t.Fatalf("OMP background launch: %v\n%s", err, ompLaunchOutput)
	}
	var ompLaunchDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(ompLaunchOutput, &ompLaunchDoc); err != nil {
		t.Fatal(err)
	}
	ompWait := exec.Command(binary, "--journal", journal, "await", ompLaunchDoc.Result.ID.String(), "--no-timeout", "--ignore-attention")
	if ompWaitOutput, err := ompWait.CombinedOutput(); err != nil {
		t.Fatalf("OMP await: %v\n%s", err, ompWaitOutput)
	}
	ompStatus := exec.Command(binary, "--journal", journal, "status", ompLaunchDoc.Result.ID.String())
	ompStatusOutput, err := ompStatus.CombinedOutput()
	if err != nil || !bytes.Contains(ompStatusOutput, []byte(`"source_state":"agent_end"`)) || !bytes.Contains(ompStatusOutput, []byte(`"label":"Read terminal result"`)) || !bytes.Contains(ompStatusOutput, []byte(`"mutates":true`)) || !bytes.Contains(ompStatusOutput, []byte(`"side_effect_class":"local_operational_write"`)) {
		t.Fatalf("OMP terminal status metadata: %v\n%s", err, ompStatusOutput)
	}
	ompResult := exec.Command(binary, "--journal", journal, "result", ompLaunchDoc.Result.ID.String(), "--require-result-source", "assistant", "--min-result-bytes", "20")
	ompResultOutput, err := ompResult.CombinedOutput()
	if err != nil || !bytes.Contains(ompResultOutput, []byte("OMP_BACKGROUND_RESULT_OK")) || !bytes.Contains(ompResultOutput, []byte(`"source":"assistant_terminal_result"`)) {
		t.Fatalf("OMP result: %v\n%s", err, ompResultOutput)
	}
	replay := exec.Command(binary, "--journal", journal, "run", "--background", "--execution-id", launchDoc.Result.ID.String(), "--adapter", "generic-process", "--", "/bin/echo", native)
	replayOutput, replayErr := replay.CombinedOutput()
	if replayErr == nil || !bytes.Contains(replayOutput, []byte(`"code":"conflict"`)) {
		t.Fatalf("existing execution ID was accepted: %v\n%s", replayErr, replayOutput)
	}
	rawRaceID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	raceID, err := ids.ParseExecutionID(rawRaceID.String())
	if err != nil {
		t.Fatal(err)
	}
	type launchResult struct {
		output []byte
		err    error
	}
	results := make(chan launchResult, 2)
	for range 2 {
		go func() {
			command := exec.Command(binary, "--journal", journal, "run", "--background", "--execution-id", raceID.String(), "--adapter", "generic-process", "--", "/bin/sh", "-c", native)
			value, runErr := command.CombinedOutput()
			results <- launchResult{output: value, err: runErr}
		}()
	}
	succeeded, conflicted := 0, 0
	observed := make([]launchResult, 0, 2)
	for range 2 {
		value := <-results
		observed = append(observed, value)
		if value.err == nil && bytes.Contains(value.output, []byte(`"ok":true`)) {
			succeeded++
		}
		if value.err != nil && bytes.Contains(value.output, []byte(`"code":"conflict"`)) {
			conflicted++
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent claim results succeeded=%d conflicted=%d observed=%#v", succeeded, conflicted, observed)
	}
	raceWait := exec.Command(binary, "--journal", journal, "await", raceID.String(), "--no-timeout", "--ignore-attention")
	if raceOutput, err := raceWait.CombinedOutput(); err != nil {
		t.Fatalf("await concurrent winner: %v\n%s", err, raceOutput)
	}
}

func TestRecentDiscoversNewestExecutionsByExactLabel(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	for _, label := range []string{"first", "review", "review"} {
		stdout.Reset()
		native := fmt.Sprintf(`{"type":"result","status":"completed","result":"%s"}`, label)
		if code := a.run(context.Background(), []string{"--journal", journal, "run", "--adapter", "generic-process", "--label", label, "--", "/bin/echo", native}); code != 0 {
			t.Fatalf("run %s exit=%d output=%s", label, code, stdout.String())
		}
		time.Sleep(time.Millisecond)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "recent", "--label", "review", "--limit", "1"}); code != 0 {
		t.Fatalf("recent exit=%d output=%s", code, stdout.String())
	}
	var doc struct {
		Result struct {
			Executions []recentExecution `json:"executions"`
			Count      int               `json:"count"`
			HasMore    bool              `json:"has_more"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Result.Count != 1 || !doc.Result.HasMore || len(doc.Result.Executions) != 1 || !reflect.DeepEqual(doc.Result.Executions[0].Labels, []string{"review"}) {
		t.Fatalf("recent=%#v", doc.Result)
	}
	if strings.Contains(stdout.String(), root) || strings.Contains(stdout.String(), `"result":"review"`) {
		t.Fatalf("recent leaked private path or result: %s", stdout.String())
	}
}

func TestRecentUnreconciledRequiresResultOrAwaitAcknowledgement(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journal, "run", "--adapter", "generic-process", "--label", "collect", "--", "/bin/echo", `{"type":"result","status":"completed","result":"COLLECT_ME"}`}); code != 0 {
		t.Fatalf("run exit=%d output=%s", code, stdout.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	id := runDoc.Result.ID.String()
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "recent", "--unreconciled", "--label", "collect"}); code != 0 {
		t.Fatalf("unreconciled exit=%d output=%s", code, stdout.String())
	}
	if !recentJSONContains(t, stdout.Bytes(), id, true) {
		t.Fatalf("completed run was not unreconciled: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "status", id}); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "recent", "--unreconciled", "--label", "collect"}); code != 0 {
		t.Fatalf("unreconciled after status exit=%d output=%s", code, stdout.String())
	}
	if !recentJSONContains(t, stdout.Bytes(), id, true) {
		t.Fatalf("status collected the result: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "result", id, "--require-result-source", "missing"}); code == 0 {
		t.Fatalf("failed result assertion succeeded: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "recent", "--unreconciled", "--label", "collect"}); code != 0 {
		t.Fatalf("unreconciled after failed result exit=%d output=%s", code, stdout.String())
	}
	if !recentJSONContains(t, stdout.Bytes(), id, true) {
		t.Fatalf("failed result assertion collected the execution: %s", stdout.String())
	}
	a.stdout = failingWriter{}
	if code := a.run(context.Background(), []string{"--journal", journal, "result", id}); code != 70 {
		t.Fatalf("failed result delivery exit=%d", code)
	}
	a.stdout = &stdout
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "recent", "--unreconciled", "--label", "collect"}); code != 0 || !recentJSONContains(t, stdout.Bytes(), id, true) {
		t.Fatalf("failed result delivery collected execution: exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "result", id}); code != 0 {
		t.Fatalf("result exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "recent", "--unreconciled", "--label", "collect"}); code != 0 {
		t.Fatalf("unreconciled after result exit=%d output=%s", code, stdout.String())
	}
	if recentJSONContains(t, stdout.Bytes(), id, true) {
		t.Fatalf("result did not acknowledge execution: %s", stdout.String())
	}

	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "run", "--adapter", "generic-process", "--label", "awaited", "--", "/bin/echo", `{"type":"result","status":"completed","result":"AWAIT_ME"}`}); code != 0 {
		t.Fatalf("awaited run exit=%d output=%s", code, stdout.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	awaitID := runDoc.Result.ID.String()
	a.stdout = failingWriter{}
	if code := a.run(context.Background(), []string{"--journal", journal, "await", awaitID, "--ignore-attention"}); code != 70 {
		t.Fatalf("failed await delivery exit=%d", code)
	}
	a.stdout = &stdout
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "recent", "--unreconciled", "--label", "awaited"}); code != 0 || !recentJSONContains(t, stdout.Bytes(), awaitID, true) {
		t.Fatalf("failed await delivery collected execution: exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "await", awaitID, "--ignore-attention"}); code != 0 {
		t.Fatalf("await exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "recent", "--unreconciled", "--label", "awaited"}); code != 0 {
		t.Fatalf("unreconciled after await exit=%d output=%s", code, stdout.String())
	}
	if recentJSONContains(t, stdout.Bytes(), awaitID, true) {
		t.Fatalf("await did not acknowledge execution: %s", stdout.String())
	}
}

func TestResultContentWritesExactStoredTextAndAcknowledges(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journal, "run", "--adapter", "generic-process", "--label", "raw-content", "--", "/bin/echo", `{"type":"result","status":"completed","result":"exact content without added newline"}`}); code != 0 {
		t.Fatalf("run exit=%d output=%s", code, stdout.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "result", runDoc.Result.ID.String(), "--content"}); code != 0 {
		t.Fatalf("content exit=%d output=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if got, want := stdout.String(), "exact content without added newline"; got != want {
		t.Fatalf("content=%q want=%q", got, want)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "recent", "--unreconciled", "--label", "raw-content"}); code != 0 {
		t.Fatalf("recent exit=%d output=%s", code, stdout.String())
	}
	if recentJSONContains(t, stdout.Bytes(), runDoc.Result.ID.String(), true) {
		t.Fatalf("content delivery did not acknowledge result: %s", stdout.String())
	}
}

func TestResultContentRejectsSummaryCombination(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"result", "exec-amber-willow-orbit-tiger-harbor-gentle", "--content", "--summary"}); code != output.ExitCodeFor(output.CodeUsage) {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
}

func TestRecentUnreconciledHidesTerminalsThatPredateAcknowledgementTracking(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "state", "journal.db")
	now := time.Now().UTC()
	past := now.Add(-2 * time.Hour)
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	legacy := model.Execution{
		Authority: model.AuthorityNative, Adapter: "generic-process", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched,
		State: model.StateCompleted, Liveness: model.LivenessExited, Labels: []string{"legacy"},
		SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: past, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		CreatedAt: past, UpdatedAt: past, TerminalAt: &past, Observation: model.Observation{Source: model.ObservationProcess, Integrity: model.IntegrityVerified, ObservedAt: past},
	}
	created, _, err := journal.CreateExecution(context.Background(), legacy, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	journal.Close()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journalPath, "recent", "--unreconciled"}); code != 0 {
		t.Fatalf("unreconciled exit=%d output=%s", code, stdout.String())
	}
	if recentJSONContains(t, stdout.Bytes(), created.ID.String(), true) {
		t.Fatalf("pre-epoch terminal appeared unreconciled: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "generic-process", "--label", "fresh", "--", "/bin/echo", `{"type":"result","status":"completed","result":"FRESH"}`}); code != 0 {
		t.Fatalf("fresh run exit=%d output=%s", code, stdout.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "recent", "--unreconciled"}); code != 0 {
		t.Fatalf("unreconciled after fresh run exit=%d output=%s", code, stdout.String())
	}
	if recentJSONContains(t, stdout.Bytes(), created.ID.String(), true) {
		t.Fatalf("pre-epoch terminal appeared after a later run: %s", stdout.String())
	}
	if !recentJSONContains(t, stdout.Bytes(), runDoc.Result.ID.String(), true) {
		t.Fatalf("fresh terminal was not unreconciled: %s", stdout.String())
	}
}

func TestParseRecentRejectsUnreconciledWithNonterminal(t *testing.T) {
	_, problem := parseRecent([]string{"--unreconciled", "--state", "nonterminal"})
	if problem == nil || problem.Code != output.CodeUsage {
		t.Fatalf("problem=%#v", problem)
	}
}

func TestRecentLivenessFilterSeparatesActiveFromUnreachable(t *testing.T) {
	acks := store.AcknowledgementIndex{}
	alive := model.Execution{Adapter: "codex", State: model.StateRunning, Liveness: model.LivenessAlive}
	unreachable := model.Execution{Adapter: "codex", State: model.StateRunning, Liveness: model.LivenessUnreachable}
	opts, problem := parseRecent([]string{"--state", "nonterminal", "--liveness", "alive"})
	if problem != nil {
		t.Fatal(problem)
	}
	if !recentMatches(alive, opts, acks) || recentMatches(unreachable, opts, acks) {
		t.Fatalf("liveness filter did not isolate active work: opts=%#v", opts)
	}
	if _, problem := parseRecent([]string{"--liveness", "stale"}); problem == nil || problem.Code != output.CodeUsage {
		t.Fatalf("invalid liveness problem=%#v", problem)
	}
}

func recentJSONContains(t *testing.T, raw []byte, executionID string, unreconciled bool) bool {
	t.Helper()
	var doc struct {
		Result struct {
			Executions []recentExecution `json:"executions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, item := range doc.Result.Executions {
		if item.ID.String() == executionID && item.Unreconciled == unreconciled {
			return true
		}
	}
	return false
}

func TestRunPromptFilePlanDoesNotExposePromptContent(t *testing.T) {
	root := t.TempDir()
	promptPath := filepath.Join(root, "task.md")
	secret := "private multi-line prompt\nsecond line"
	if err := os.WriteFile(promptPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--plan", "--cwd", root, "--prompt-file", "task.md", "--prompt-delivery", "argv", "--adapter", "generic-process", "--", "/bin/echo"}); code != 0 {
		t.Fatalf("plan exit=%d output=%s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stdout.String(), "second line") {
		t.Fatalf("plan leaked prompt content: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), promptPath) {
		t.Fatalf("plan leaked absolute prompt path: %s", stdout.String())
	}
	for _, want := range []string{`"delivery":"argv"`, `"bytes":37`, `"sha256":"sha256:`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("plan missing %s: %s", want, stdout.String())
		}
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("plan created journal: %v", err)
	}
}

func TestRunPromptStdinDeliveryStoresResult(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "stdin-agent")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nIFS= read -r prompt\nprintf '%s\\n' \"{\\\"type\\\":\\\"result\\\",\\\"status\\\":\\\"completed\\\",\\\"result\\\":\\\"$prompt\\\"}\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("PROMPT_STDIN_OK\n")
	a.stdinIsTerminal = func() bool { return false }
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--adapter", "generic-process", "--prompt-stdin", "--prompt-delivery", "stdin", "--", script}); code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "result", runDoc.Result.ID.String()}); code != 0 || !strings.Contains(stdout.String(), "PROMPT_STDIN_OK") {
		t.Fatalf("result exit=%d output=%s", code, stdout.String())
	}
}

func TestRunPromptFlagsFailClosed(t *testing.T) {
	tests := [][]string{
		{"--prompt-file", "task.md", "--prompt-stdin", "--", "/bin/echo"},
		{"--prompt-delivery", "stdin", "--", "/bin/echo"},
		{"--timeout", "1h", "--no-timeout", "--", "/bin/echo"},
	}
	for _, args := range tests {
		if _, problem := parseRun(args); problem == nil || problem.Code != output.CodeUsage {
			t.Fatalf("args=%v problem=%#v", args, problem)
		}
	}
}

func TestFanoutRunsOnePromptThroughArgvAndStdin(t *testing.T) {
	root := t.TempDir()
	promptPath := filepath.Join(root, "task.md")
	if err := os.WriteFile(promptPath, []byte("FANOUT_PROMPT_OK"), 0o600); err != nil {
		t.Fatal(err)
	}
	argvScript := filepath.Join(root, "argv-agent")
	stdinScript := filepath.Join(root, "stdin-agent")
	if err := os.WriteFile(argvScript, []byte("#!/bin/sh\nprintf '%s\\n' \"{\\\"type\\\":\\\"result\\\",\\\"status\\\":\\\"completed\\\",\\\"result\\\":\\\"$1\\\"}\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stdinScript, []byte("#!/bin/sh\nIFS= read -r prompt\nprintf '%s\\n' \"{\\\"type\\\":\\\"result\\\",\\\"status\\\":\\\"completed\\\",\\\"result\\\":\\\"$prompt\\\"}\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "fanout.json")
	manifest := fmt.Sprintf(`{"schema_version":1,"prompt_file":"task.md","concurrency":2,"children":[{"adapter":"generic-process","prompt_delivery":"argv","argv":[%q]},{"adapter":"generic-process","prompt_delivery":"stdin","argv":[%q]}]}`, argvScript, stdinScript)
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "fanout", "--manifest", manifestPath}); code != 0 {
		t.Fatalf("fanout exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var doc struct {
		Result fanoutResult `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Result.Success || len(doc.Result.Children) != 2 {
		t.Fatalf("fanout result=%#v", doc.Result)
	}
	for _, child := range doc.Result.Children {
		stdout.Reset()
		if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "result", child.ExecutionID}); code != 0 || !strings.Contains(stdout.String(), "FANOUT_PROMPT_OK") {
			t.Fatalf("child %s result exit=%d output=%s", child.ExecutionID, code, stdout.String())
		}
	}
}

func TestFanoutManifestRejectsInvalidChildControls(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "task.md"), []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make([]string, 257)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	tests := []fanoutManifest{
		{SchemaVersion: 1, PromptFile: "task.md", Children: []fanoutChild{{Argv: tooMany}}},
		{SchemaVersion: 1, PromptFile: "task.md", Children: []fanoutChild{{Timeout: "later", Argv: []string{"echo"}}}},
		{SchemaVersion: 1, PromptFile: "task.md", Children: []fanoutChild{{ExecutionID: rawID.String(), Argv: []string{"echo"}}, {ExecutionID: rawID.String(), Argv: []string{"echo"}}}},
		{SchemaVersion: 1, PromptFile: "task.md", PromptDelivery: "magic", Children: []fanoutChild{{PromptDelivery: "argv", Argv: []string{"echo"}}}},
	}
	for i, manifest := range tests {
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, fmt.Sprintf("fanout-%d.json", i))
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, problem := readFanoutManifest(path); problem == nil || problem.Code != output.CodeUsage {
			t.Fatalf("case %d problem=%#v", i, problem)
		}
	}
}

func TestPromptAndManifestRejectSymlinks(t *testing.T) {
	root := t.TempDir()
	realPrompt := filepath.Join(root, "real.md")
	if err := os.WriteFile(realPrompt, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedPrompt := filepath.Join(root, "linked.md")
	if err := os.Symlink(realPrompt, linkedPrompt); err != nil {
		t.Fatal(err)
	}
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	if _, problem := a.loadPrompt(runOptions{cwd: root, promptFile: "linked.md", promptDelivery: "argv"}); problem == nil || problem.Code != output.CodeAuthorizationDenied {
		t.Fatalf("prompt symlink problem=%#v", problem)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "escaped.md"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, problem := a.loadPrompt(runOptions{cwd: root, promptFile: filepath.Join(outside, "escaped.md"), promptDelivery: "argv"}); problem == nil || problem.Code != output.CodeAuthorizationDenied || problem.Details["recommended_prompt_source"] != "stdin" || len(problem.NextActions) == 0 {
		t.Fatalf("cross-root prompt did not explain the stdin remedy: %#v", problem)
	}
	if err := os.Symlink(outside, filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	if _, problem := a.loadPrompt(runOptions{cwd: root, promptFile: filepath.Join("nested", "escaped.md"), promptDelivery: "argv"}); problem == nil || problem.Code != output.CodeAuthorizationDenied {
		t.Fatalf("nested prompt symlink problem=%#v", problem)
	}
	realManifest := filepath.Join(root, "fanout.json")
	if err := os.WriteFile(realManifest, []byte(`{"schema_version":1,"prompt_file":"real.md","children":[{"argv":["echo"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedManifest := filepath.Join(root, "linked.json")
	if err := os.Symlink(realManifest, linkedManifest); err != nil {
		t.Fatal(err)
	}
	if _, _, problem := readFanoutManifest(linkedManifest); problem == nil || problem.Code != output.CodeAuthorizationDenied {
		t.Fatalf("manifest symlink problem=%#v", problem)
	}
}

func TestOwnedRunSurvivesJournalLockWhileChildTerminalizes(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "started")
	gate := filepath.Join(root, "release")
	script := filepath.Join(root, "agent")
	body := fmt.Sprintf("#!/bin/sh\n: > %q\nwhile [ ! -f %q ]; do sleep 0.02; done\nprintf '%%s\\n' '{\"type\":\"result\",\"status\":\"completed\",\"result\":\"LOCK_SURVIVED\"}'\n", marker, gate)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	rawID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	done := make(chan int, 1)
	go func() {
		done <- a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "run", "--execution-id", rawID.String(), "--adapter", "generic-process", "--", script})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	lock, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	select {
	case code := <-done:
		lock.Close()
		t.Fatalf("run returned under journal contention: exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	case <-time.After(2500 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
		}
		stdout.Reset()
		if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "result", rawID.String()}); code != 0 || !strings.Contains(stdout.String(), "LOCK_SURVIVED") {
			t.Fatalf("result exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("run did not commit terminal result after journal lock released")
	}
}

func TestCursorPlanModeFailsClosedBeforeProbeOrJournal(t *testing.T) {
	for _, native := range [][]string{{"cursor-agent", "--print", "--plan", "review"}, {"cursor-agent", "--mode", "plan", "review"}, {"cursor-agent", "--mode=plan", "review"}} {
		journal := filepath.Join(t.TempDir(), "journal.db")
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		args := append([]string{"--journal", journal, "run", "--"}, native...)
		if code := a.run(context.Background(), args); code != output.ExitCodeFor(output.CodeCapabilityUnavailable) || !strings.Contains(stdout.String(), `"diagnostic_code":"cursor_plan_result_unreliable"`) {
			t.Fatalf("plan mode exit=%d output=%s", code, stdout.String())
		}
		if _, err := os.Stat(journal); !os.IsNotExist(err) {
			t.Fatalf("rejected plan mode created journal: %v", err)
		}
	}

	opts, problem := parseRun([]string{"--allow-unreliable-result", "--", "cursor-agent", "--plan", "review"})
	if problem != nil || !opts.allowUnreliableResult {
		t.Fatalf("explicit override was not parsed: opts=%#v problem=%v", opts, problem)
	}
}

func TestOMPResultModeFailsClosedBeforeProbeOrJournal(t *testing.T) {
	for _, native := range [][]string{{"omp", "-p", "review"}, {"omp", "-p", "--mode", "text", "review"}, {"omp", "-p", "--mode=rpc", "review"}} {
		journal := filepath.Join(t.TempDir(), "journal.db")
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		args := append([]string{"--journal", journal, "run", "--"}, native...)
		if code := a.run(context.Background(), args); code != output.ExitCodeFor(output.CodeCapabilityUnavailable) || !strings.Contains(stdout.String(), `"diagnostic_code":"invocation_output_mode_required"`) || !strings.Contains(stdout.String(), `"required_argv"`) {
			t.Fatalf("OMP mode exit=%d output=%s", code, stdout.String())
		}
		if _, err := os.Stat(journal); !os.IsNotExist(err) {
			t.Fatalf("rejected OMP mode created journal: %v", err)
		}
	}
	for _, argv := range [][]string{{"omp", "-p", "--mode", "json", "review"}, {"omp", "--mode=JSON", "review"}, {"omp", "--mode", "text", "--mode=json", "review"}} {
		if !ompJSONMode(argv) {
			t.Fatalf("OMP JSON mode not recognized: %v", argv)
		}
	}
	for _, argv := range [][]string{{"omp", "--mode", "text", "review"}, {"omp", "--mode=json", "--mode", "text", "review"}, {"omp", "--", "--mode", "json"}} {
		if ompJSONMode(argv) {
			t.Fatalf("OMP non-JSON mode was accepted as reliable: %v", argv)
		}
	}
	opts, problem := parseRun([]string{"--allow-missing-result", "--", "omp", "-p", "review"})
	if problem != nil || !opts.allowMissingResult {
		t.Fatalf("explicit missing-result acceptance was not parsed: opts=%#v problem=%v", opts, problem)
	}
}

func TestKnownAdapterStructuredOutputFailsClosedBeforeProbeOrJournal(t *testing.T) {
	for _, tc := range []struct {
		adapter string
		argv    []string
	}{
		{adapter: "codex", argv: []string{"missing-codex-binary", "exec", "review"}},
		{adapter: "cursor", argv: []string{"missing-cursor-binary", "--print", "review"}},
		{adapter: "cursor", argv: []string{"missing-cursor-binary", "--print", "--output-format", "stream-json", "--output-format", "text", "review"}},
		{adapter: "omp", argv: []string{"missing-omp-binary", "-p", "--mode=json", "--mode", "text", "review"}},
	} {
		journal := filepath.Join(t.TempDir(), "journal.db")
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		args := []string{"--journal", journal, "run", "--adapter", tc.adapter, "--"}
		args = append(args, tc.argv...)
		if code := a.run(context.Background(), args); code != output.ExitCodeFor(output.CodeCapabilityUnavailable) || !strings.Contains(stdout.String(), `"diagnostic_code":"invocation_output_mode_required"`) || !strings.Contains(stdout.String(), `"required_argv"`) {
			t.Fatalf("adapter=%s exit=%d output=%s", tc.adapter, code, stdout.String())
		}
		if _, err := os.Stat(journal); !os.IsNotExist(err) {
			t.Fatalf("adapter=%s rejected invocation created journal: %v", tc.adapter, err)
		}
	}
}

func TestCleanExitWithoutTerminalJournalsExtractionFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", "plain output"}); code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	if runDoc.Result.State != model.StateOrphaned {
		t.Fatalf("state=%s output=%s", runDoc.Result.State, stdout.String())
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := journal.GetOutcome(context.Background(), runDoc.Result.ID)
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Failure == nil || outcome.Failure.Code != "result_extraction_failed" || outcome.Failure.Kind != "observation" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestAssistantResultSourceAcceptsCursorTerminalAndFallback(t *testing.T) {
	for _, source := range []string{"assistant", "assistant_terminal_result", "assistant_message_fallback"} {
		if !resultSourceSatisfies(source, "assistant") {
			t.Fatalf("assistant requirement rejected %q", source)
		}
	}
	if resultSourceSatisfies("terminal_result", "assistant") {
		t.Fatal("generic terminal result satisfied assistant requirement")
	}
}

func TestRunTimeoutTerminalizesExecution(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--timeout", "300ms", "--allow-missing-result", "--", "/bin/sh", "-c", "sleep 2"})
	if code != output.ExitCodeFor(output.CodeTimeout) {
		t.Fatalf("timeout exit=%d output=%s", code, stdout.String())
	}
	var doc output.ErrorDocument
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	executionID, _ := doc.Error.Details["execution_id"].(string)
	id, err := ids.ParseExecutionID(executionID)
	if err != nil {
		t.Fatalf("timeout omitted execution ID: %s", stdout.String())
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	execution, err := journal.GetExecution(context.Background(), id)
	if err != nil || !execution.State.Terminal() {
		t.Fatalf("timed out execution=%#v err=%v", execution, err)
	}
}

func TestRunInterruptionTerminalizesExecutionAsCancelled(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	rawExecutionID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	executionID, err := ids.ParseExecutionID(rawExecutionID.String())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- a.run(ctx, []string{"--journal", journalPath, "run", "--execution-id", executionID.String(), "--allow-missing-result", "--", "/bin/sh", "-c", "sleep 2"})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		journal, openErr := store.Open(journalPath, store.Options{ReadOnly: true})
		if openErr == nil {
			execution, getErr := journal.GetExecution(context.Background(), executionID)
			journal.Close()
			if getErr == nil && execution.State == model.StateRunning {
				break
			}
		}
		select {
		case code := <-done:
			t.Fatalf("run terminated before launch acceptance: exit=%d output=%s", code, stdout.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("execution did not reach the accepted running state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	var code int
	select {
	case code = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted run did not terminate")
	}
	if code != output.ExitCodeFor(output.CodeExecutionCancelled) {
		t.Fatalf("interruption exit=%d output=%s", code, stdout.String())
	}
	var doc output.ErrorDocument
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	documentedID, _ := doc.Error.Details["execution_id"].(string)
	id, err := ids.ParseExecutionID(documentedID)
	if err != nil || id != executionID {
		t.Fatalf("interruption omitted execution ID: %s", stdout.String())
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	execution, err := journal.GetExecution(context.Background(), id)
	if err != nil || execution.State != model.StateCancelled {
		t.Fatalf("interrupted execution=%#v err=%v", execution, err)
	}
	events, err := journal.ListEvents(context.Background(), id, contracts.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	terminal := 0
	for _, event := range events {
		if event.Kind == model.EventTerminal {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal event count=%d events=%#v", terminal, events)
	}
}

func TestNativeAttentionUpdatesExecutionAndStopsAwait(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "state", "journal.db")
	script := filepath.Join(root, "attention-agent")
	blocker := filepath.Join(root, "attention-blocker")
	contents := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"session.started\",\"session_id\":\"attention-fixture\"}' '{\"type\":\"permission.required\",\"message\":\"must-not-persist\"}'\nmkfifo \"$1\"\nexec cat \"$1\"\n"
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
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- runner.run(runCtx, []string{"--output", "json", "--journal", journalPath, "run", "--adapter", "generic-process", "--execution-id", executionID.String(), "--", script, blocker})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		journal, openErr := store.Open(journalPath, store.Options{ReadOnly: true})
		if openErr == nil {
			execution, getErr := journal.GetExecution(context.Background(), executionID)
			journal.Close()
			if getErr == nil && execution.State == model.StateAttention {
				if execution.Liveness != model.LivenessBlocked {
					t.Fatalf("attention liveness=%s", execution.Liveness)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution did not reach attention; run_output=%s", runOut.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	var awaitOut, awaitErr bytes.Buffer
	observer := testApp(&awaitOut, &awaitErr)
	if code := observer.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "await", executionID.String(), "--timeout", "1s"}); code != output.ExitCodeFor(output.CodeAttentionRequired) {
		t.Fatalf("await exit=%d output=%s stderr=%s", code, awaitOut.String(), awaitErr.String())
	}
	cancel()
	select {
	case code := <-done:
		if code != output.ExitCodeFor(output.CodeExecutionCancelled) {
			t.Fatalf("attention run cancellation exit=%d output=%s", code, runOut.String())
		}
	case <-time.After(7 * time.Second):
		t.Fatal("attention run did not terminate after cancellation")
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := journal.GetExecution(context.Background(), executionID)
	journal.Close()
	if err != nil || terminal.State != model.StateCancelled {
		t.Fatalf("cancelled attention execution=%#v err=%v", terminal, err)
	}
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("must-not-persist")) {
		t.Fatal("attention prompt entered journal")
	}
}

func TestAwaitContextCancellationIsNotReportedAsTimeout(t *testing.T) {
	now := time.Now().UTC()
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{Authority: model.AuthorityNative, Adapter: "generic-process", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}}, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- a.run(ctx, []string{"--output", "json", "--journal", journalPath, "await", execution.ID.String(), "--no-timeout"})
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != output.ExitCodeFor(output.CodeExecutionCancelled) || !strings.Contains(stdout.String(), `"code":"execution_cancelled"`) {
			t.Fatalf("await exit=%d output=%s", code, stdout.String())
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled await did not return")
	}
}

func TestContextBoundJournalOpenDoesNotOverrunDeadline(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	owner, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	journal, err := openJournalWithRetryContext(ctx, journalPath, store.Options{})
	if journal != nil {
		journal.Close()
		t.Fatal("context-bound open unexpectedly acquired a locked journal")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context-bound open error=%v", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 750*time.Millisecond {
		t.Fatalf("context-bound journal open overran deadline: %s", elapsed)
	}
}

func TestRunCancelledBeforeLaunchDoesNotCreateExecution(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code := a.run(ctx, []string{"--journal", journalPath, "run", "--allow-missing-result", "--", "/bin/sh", "-c", "sleep 2"})
	if code != output.ExitCodeFor(output.CodeExecutionCancelled) || !strings.Contains(stdout.String(), `"code":"execution_cancelled"`) {
		t.Fatalf("pre-launch interruption exit=%d output=%s", code, stdout.String())
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("pre-launch interruption created a journal: %v", err)
	}
}

func TestAwaitThroughExecutionDeadlineUsesRecordedDeadline(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "journal.db")
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	deadline := now.Add(-11 * time.Second)
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{Authority: model.AuthorityNative, Adapter: "generic-process", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}}, DeadlineAt: &deadline, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	started := time.Now()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "await", execution.ID.String(), "--through-execution-deadline"}); code != output.ExitCodeFor(output.CodeTimeout) {
		t.Fatalf("deadline await exit=%d output=%s", code, stdout.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("expired execution deadline waited %s", elapsed)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "await", execution.ID.String(), "--timeout", "1s", "--through-execution-deadline"}); code != output.ExitCodeFor(output.CodeUsage) {
		t.Fatalf("mutually exclusive await flags exit=%d output=%s", code, stdout.String())
	}
}

func TestAwaitThroughExecutionDeadlineRequiresRecordedDeadline(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "journal.db")
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{Authority: model.AuthorityNative, Adapter: "generic-process", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}}, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journalPath, "await", execution.ID.String(), "--through-execution-deadline"}); code != output.ExitCodeFor(output.CodeCapabilityUnavailable) || !strings.Contains(stdout.String(), `"diagnostic_code":"execution_deadline_unavailable"`) {
		t.Fatalf("missing deadline exit=%d output=%s", code, stdout.String())
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

func TestRouteExplainRejectsNeedsPR(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"route", "explain", "--needs-pr", "--output", "json"})
	if code == 0 {
		t.Fatalf("expected usage error, got %s", stdout.String())
	}
}

func TestRouteExplainRejectsEmptySelector(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"route", "explain"}); code != output.ExitCodeFor(output.CodeUsage) {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"code":"usage"`) || !strings.Contains(stdout.String(), `requires a host or model selector`) {
		t.Fatalf("empty selector did not fail explicitly: %s", stdout.String())
	}
}

func TestRouteExplainQueryJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"route", "explain", "--output", "json", "--", "glm"})
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
	if !doc.OK {
		t.Fatalf("doc=%#v", doc)
	}
	if _, ok := doc.Result["query"]; ok {
		t.Fatalf("query should not be echoed: %#v", doc.Result)
	}
	if _, ok := doc.Result["hosts"]; ok {
		t.Fatalf("empty hosts should be omitted: %#v", doc.Result)
	}
	models, _ := doc.Result["models"].([]any)
	if len(models) == 0 {
		t.Fatalf("expected model hits: %#v", doc.Result)
	}
}

func TestRouteExplainWarnsWhenTaskProseIsIgnored(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"route", "explain", "--", "open", "a", "PR", "with", "grok"}); code != 0 {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"code":"route_unmatched_tokens"`) || !strings.Contains(stdout.String(), `"tokens":["open","pr"]`) || !strings.Contains(stdout.String(), `"adapter":"cursor"`) {
		t.Fatalf("mixed selector did not make ignored prose explicit: %s", stdout.String())
	}
}

func TestRouteExplainFailsClosedForExplicitMissingConfigAndProfile(t *testing.T) {
	for _, args := range [][]string{
		{"--config", filepath.Join(t.TempDir(), "missing.json"), "route", "explain", "m5", "grok"},
		{"--profile", "missing", "route", "explain", "m5", "grok"},
	} {
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		if code := a.run(context.Background(), args); code == 0 {
			t.Fatalf("args=%v unexpectedly succeeded: %s", args, stdout.String())
		}
		if !strings.Contains(stdout.String(), `"ok":false`) {
			t.Fatalf("args=%v missing structured error: %s", args, stdout.String())
		}
	}
}

func TestRouteExplainRemoteIsExplicitlyNotDispatched(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	raw := `{"schema_version":1,"default_profile":"fleet","profiles":{"fleet":{"agent_preferences":{"mode":"advisory","preferred":[{"agent":"codex","model":"gpt-5.6-sol","speed":"regular","use_for":"alias:sol"}]},"route":{"this_host":"m4-air","hosts":{"m5":"m5-mbp"},"placement":{"kind":"multica"}}}}}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--config", configPath, "route", "explain", "m5", "sol"}); code != 0 {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"code":"route_not_dispatched"`) || !strings.Contains(stdout.String(), `"tracked_execution":false`) || !strings.Contains(stdout.String(), `"agentctl","dispatch"`) || !strings.Contains(stdout.String(), `"--plan"`) {
		t.Fatalf("remote advice looked dispatched: %s", stdout.String())
	}
}

func TestCapabilitiesSummaryCanRequireResultContent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"--output", "json", "capabilities", "codex", "--static", "--require", "launch,result_content"})
	if code != 0 {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"viable":true`) || !strings.Contains(stdout.String(), `"result_content"`) || strings.Contains(stdout.String(), `"manifest"`) {
		t.Fatalf("unexpected summary: %s", stdout.String())
	}
}

func TestNativeRunPersistsRetrievableOutcomeAndRedactsPrivateSource(t *testing.T) {
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
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "result", doc.Result.ID.String(), "--require-content"}); code != 0 {
		t.Fatalf("result exit=%d output=%s", code, stdout.String())
	}
	var resultDoc struct {
		Result struct {
			Outcome model.Outcome `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resultDoc); err != nil {
		t.Fatal(err)
	}
	if resultDoc.Result.Outcome.Content == nil || resultDoc.Result.Outcome.Content.Text != "must-not-enter-journal" || resultDoc.Result.Outcome.ResultRef == "" {
		t.Fatalf("outcome=%#v", resultDoc.Result.Outcome)
	}
}

func TestNoStoreResultWritesExplicitOutcomeTombstone(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	journal := filepath.Join(t.TempDir(), "state", "journal.db")
	secret := "do-not-persist-this-result"
	native := `{"type":"result","status":"completed","result":"` + secret + `"}`
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--adapter", "generic-process", "--no-store-result", "--", "/bin/echo", native}); code != 0 {
		t.Fatalf("run exit=%d output=%s", code, stdout.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "result", runDoc.Result.ID.String()}); code != 3 || !strings.Contains(stdout.String(), `"availability":"omitted_by_policy"`) {
		t.Fatalf("default result exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "result", runDoc.Result.ID.String(), "--allow-empty"}); code != 0 {
		t.Fatalf("result exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"availability":"omitted_by_policy"`) || strings.Contains(stdout.String(), secret) {
		t.Fatalf("unexpected tombstone: %s", stdout.String())
	}
	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("no-store result bytes entered journal")
	}
}

func TestFailedRunResultReturnsStoredFailureWithoutAllowEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	journal := filepath.Join(t.TempDir(), "state", "journal.db")
	native := `{"type":"result","status":"failed","error":"permission denied"}`
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--adapter", "generic-process", "--", "/bin/echo", native}); code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "result", runDoc.Result.ID.String()}); code != 0 {
		t.Fatalf("result exit=%d output=%s", code, stdout.String())
	}
	var resultDoc struct {
		Result struct {
			Outcome model.Outcome `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resultDoc); err != nil {
		t.Fatal(err)
	}
	if resultDoc.Result.Outcome.Failure == nil || resultDoc.Result.Outcome.Failure.Message != "permission denied" {
		t.Fatalf("outcome=%#v", resultDoc.Result.Outcome)
	}
}

func TestResultRejectsConflictedTerminalEvidence(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	native := `{"type":"result","status":"completed","result":"answer"}`
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", native}); code != 0 {
		t.Fatalf("run exit=%d output=%s", code, stdout.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := journal.GetExecution(context.Background(), runDoc.Result.ID)
	if err != nil {
		t.Fatal(err)
	}
	execution.Observation.Integrity = model.IntegrityConflicted
	execution.UpdatedAt = execution.UpdatedAt.Add(time.Second)
	execution.Observation.ObservedAt = execution.UpdatedAt
	if _, err := journal.UpdateExecution(context.Background(), execution, execution.Revision); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "result", runDoc.Result.ID.String()}); code != 13 || !strings.Contains(stdout.String(), `"code":"unknown_state"`) {
		t.Fatalf("result exit=%d output=%s", code, stdout.String())
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

func TestNativeRunRenewsLeaseWhileReapingAfterEarlyResult(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "state", "journal.db")
	script := filepath.Join(root, "slow-exit-agent")
	contents := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"status\":\"completed\",\"result\":\"done\"}'\nsleep 7\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	rawID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	executionID, err := ids.ParseExecutionID(rawID.String())
	if err != nil {
		t.Fatal(err)
	}
	var runOut, runErr bytes.Buffer
	runner := testApp(&runOut, &runErr)
	done := make(chan int, 1)
	go func() {
		done <- runner.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "generic-process", "--execution-id", executionID.String(), "--", script})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		journal, openErr := store.Open(journalPath, store.Options{ReadOnly: true})
		if openErr == nil {
			execution, getErr := journal.GetExecution(context.Background(), executionID)
			journal.Close()
			if getErr == nil && execution.State == model.StateRunning && execution.Liveness == model.LivenessAlive {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("early-result execution did not become observable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(6 * time.Second)
	var supervisorOut, supervisorErr bytes.Buffer
	supervisorApp := testApp(&supervisorOut, &supervisorErr)
	if code := supervisorApp.run(context.Background(), []string{"--journal", journalPath, "supervisor", "run", "--once", "--state-dir", filepath.Join(root, "supervisor")}); code != 0 {
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
	if live.State != model.StateRunning || live.Liveness != model.LivenessAlive || live.Observation.Source != model.ObservationNativeStream || live.Observation.Integrity != model.IntegrityVerified {
		t.Fatalf("supervisor corrupted lease during native exit cleanup: %#v", live)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run exit=%d output=%s stderr=%s", code, runOut.String(), runErr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not finish after slow exit")
	}
}

func TestCursorRunStoresAssistantFallbackAndDrainsFinalEvents(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "state", "journal.db")
	script := filepath.Join(root, "cursor-empty-result")
	contents := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"cursor-fallback\"}' '{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"fallback answer\"}]}}' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}'\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "cursor", "--", script, "--output-format", "stream-json"}); code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var runDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "result", runDoc.Result.ID.String()}); code != 0 || !strings.Contains(stdout.String(), `"text":"fallback answer"`) {
		t.Fatalf("fallback result exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "result", runDoc.Result.ID.String(), "--require-result-source", "assistant", "--min-result-bytes", "15"}); code != 0 || !strings.Contains(stdout.String(), `"source":"assistant_message_fallback"`) {
		t.Fatalf("result guarantees exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "result", runDoc.Result.ID.String(), "--min-result-bytes", "100"}); code != output.ExitCodeFor(output.CodeInvalidState) || !strings.Contains(stdout.String(), `"actual_bytes":15`) {
		t.Fatalf("minimum result assertion exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "result", runDoc.Result.ID.String(), "--require-result-source", "terminal_result"}); code != output.ExitCodeFor(output.CodeInvalidState) || !strings.Contains(stdout.String(), `"actual_source":"assistant_message_fallback"`) {
		t.Fatalf("result source assertion exit=%d output=%s", code, stdout.String())
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	events, err := journal.ListEvents(context.Background(), runDoc.Result.ID, contracts.EventQuery{})
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	var sawAssistant, sawTerminal bool
	for _, event := range events {
		if event.SourceState != nil && *event.SourceState == "assistant" {
			sawAssistant = true
		}
		if event.Kind == model.EventTerminal {
			sawTerminal = true
			if event.Payload["diagnostic_code"] != "empty_terminal_result" || event.Payload["result_content_source"] != "assistant_message_fallback" {
				t.Fatalf("terminal event has wrong fallback metadata: %#v", event.Payload)
			}
		}
	}
	if !sawAssistant || !sawTerminal {
		t.Fatalf("missing assistant or terminal event: %#v", events)
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
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "subscribe", "create", "--execution", runDoc.Result.ID.String(), "--destination", "file", "--target", eventPath}); code != 0 {
		t.Fatalf("subscribe exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"subscription"`) || !strings.Contains(stdout.String(), `"sub-`) || !strings.Contains(stdout.String(), `"attention"`) || !strings.Contains(stdout.String(), `"artifact"`) || !strings.Contains(stdout.String(), `"terminal"`) {
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

func TestPromotionSendsContentBoundProvenanceOnStdin(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "state", "journal.db")
	configPath := filepath.Join(root, "config", "config.json")
	fakeMultica := filepath.Join(root, "fake-multica")
	capture := filepath.Join(root, "captured-description")
	skillPath := filepath.Join(root, "skill", "SKILL.md")
	contextPath := filepath.Join(root, "context.md")
	handoffPath := filepath.Join(root, "handoff.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{skillPath: "portable instructions\n", contextPath: "selected context\n", handoffPath: "bounded handoff\n"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(fakeMultica, []byte("#!/bin/sh\ncat >\"$AGENTCTL_TEST_CAPTURE\"\nprintf '%s\\n' '{\"id\":\"opaque-multica-uuid\",\"identifier\":\"SCA-888\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(previousCWD) }()
	t.Setenv("AGENTCTL_TEST_CAPTURE", capture)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.getenv = func(key string) string {
		switch key {
		case "AGENTCTL_SKILL_PATH":
			return skillPath
		case "HOME":
			return root
		default:
			return ""
		}
	}
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
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "--config", configPath, "--profile", "fleet", "--context-file", contextPath, "promote", runDoc.Result.ID.String(), "--title", "Continue durable work", "--handoff-file", handoffPath}); code != 0 {
		t.Fatalf("promote exit=%d output=%s", code, stdout.String())
	}
	body, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bounded handoff", "source_execution: " + runDoc.Result.ID.String(), sha256Digest([]byte("portable instructions\n")), sha256Digest([]byte("selected context\n")), sha256Digest([]byte("bounded handoff\n"))} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("description omitted %q: %s", want, body)
		}
	}
	if strings.Contains(string(body), root) {
		t.Fatalf("description leaked local path: %s", body)
	}
	var doc struct {
		Result struct {
			Execution  model.Execution           `json:"execution"`
			Provenance model.ExecutionProvenance `json:"provenance"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Result.Execution.TaskContract == nil || doc.Result.Execution.TaskContract.Provenance == nil || doc.Result.Provenance.PortableSkillDigest == "" {
		t.Fatalf("promotion omitted durable provenance: %s", stdout.String())
	}
}
