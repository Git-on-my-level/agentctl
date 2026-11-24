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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/admission"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func admissionFixture(t *testing.T, enabled bool, maxWait string) (delegateFixture, string) {
	t.Helper()
	f := newDelegateFixture(t, "")
	script := filepath.Join(f.root, "opencode")
	body := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo 'opencode 2.3.2'
  exit 0
fi
[ -z "${AGENTCTL_ADMISSION_PRIORITY+x}" ] || exit 23
echo launch >> launches
printf '%s\n' '{"type":"retry","sessionID":"ses_admission","attempt":1,"error":{"name":"provider.rate-limit","status":429,"message":"private-provider-text"}}' '{"type":"step_start","sessionID":"ses_admission","part":{"type":"step-start"}}' '{"type":"text","sessionID":"ses_admission","part":{"type":"text","text":"fixture answer","time":{"start":1,"end":2}}}' '{"type":"step_finish","sessionID":"ses_admission","part":{"type":"step-finish","reason":"stop"}}'
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{"test": {
		Adapters:         map[string]config.Adapter{"opencode": {Executable: script}},
		AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: "opencode", Model: "morph/morph-dsv41flash"}}},
		Admission:        &admission.Config{Buckets: map[string]admission.Bucket{"morph": {Harness: "opencode", Model: "morph/morph-dsv41flash", Enabled: &enabled, MinWindow: 1, InitialWindow: 1, MaxWindow: 1, MaxWait: maxWait}}},
	}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.selectRequest(t, `{"harness":"opencode","model":"morph/morph-dsv41flash"}`)
	return f, script
}

func TestProviderAdmissionDetachedQueueCancelAndDeadline(t *testing.T) {
	t.Parallel()
	f, _ := admissionFixture(t, true, "90s")
	binary := filepath.Join(t.TempDir(), "agentctl")
	if out, err := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	command := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, append([]string{"--config", f.config, "--journal", f.journal}, args...)...)
		cmd.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off", "AGENTCTL_ADMISSION_PRIORITY=interactive")
		cmd.Stdin = strings.NewReader("private task prompt")
		return cmd
	}
	invoke := func(args ...string) string {
		t.Helper()
		out, err := command(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%q: %v %s", args, err, out)
		}
		return string(out)
	}
	decode := func(out string) model.Execution {
		t.Helper()
		var result struct{ Result model.Execution }
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return result.Result
	}
	pid := strconv.Itoa(os.Getpid())
	if out := invoke("admission", "acquire", "--bucket", "morph", "--holder", "multica:fixture", "--pid", pid, "--no-wait"); !strings.Contains(out, `"granted":true`) {
		t.Fatal(out)
	}
	args := []string{"delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--timeout", "5s"}
	queued := decode(invoke(append(args, "--detach")...))
	if !queued.ProviderQueued() || queued.DeadlineAt != nil || queued.Admission.Priority != "interactive" {
		t.Fatalf("queued receipt: %#v", queued)
	}
	status := func() model.Execution { return decode(invoke("status", queued.ID.String())) }
	queued = status()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, binary, "--config", f.config, "--journal", f.journal, "cancel", queued.ID.String(), "--timeout", "5s").Run()
	}()
	collect := command(append(args, "--wait", "--content")...)
	await := command("await", queued.ID.String(), "--through-execution-deadline", "--content")
	var collectOut, awaitOut bytes.Buffer
	for _, item := range []struct {
		cmd *exec.Cmd
		out *bytes.Buffer
	}{{collect, &collectOut}, {await, &awaitOut}} {
		item.cmd.Stdout, item.cmd.Stderr = item.out, item.out
		if err := item.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func(cmd *exec.Cmd) {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}(item.cmd)
	}
	other := filepath.Join(f.root, "cancel-request.json")
	if err := os.WriteFile(other, []byte(`{"schema_version":1,"request_key":"cancel-fixture","selector":{"harness":"opencode","model":"morph/morph-dsv41flash"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cancelled := decode(invoke("delegate", "--cwd", f.root, "--request-file", other, "--prompt-stdin", "--detach"))
	if !cancelled.ProviderQueued() {
		t.Fatalf("second receipt: %#v", cancelled)
	}
	cancelled = decode(invoke("cancel", cancelled.ID.String(), "--timeout", "5s"))
	if cancelled.State != model.StateCancelled || cancelled.Admission.Phase != "cancelled" || cancelled.Launch != nil {
		t.Fatalf("cancel: %#v", cancelled)
	}
	// The default case exercises real processes and cancellation without
	// adding a minute to every suite. Release qualification can run a real
	// hold beyond the old cutoff using ADMISSION_TEST_REAL_QUEUE_WAIT=1.
	hold := time.Second
	if os.Getenv("ADMISSION_TEST_REAL_QUEUE_WAIT") == "1" {
		hold = 62 * time.Second
	}
	until := queued.CreatedAt.Add(hold)
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	for time.Now().Before(until) {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-poll.C:
		}
	}
	current := status()
	if !current.ProviderQueued() || current.Admission.Position != 1 || current.DeadlineAt != nil || time.Since(current.CreatedAt) < hold {
		t.Fatalf("long queue: %#v", current)
	}
	if _, err := os.Stat(f.count); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native process launched during queue: %v", err)
	}
	if out := invoke("recent"); !strings.Contains(out, `"phase":"queued"`) {
		t.Fatal(out)
	}
	invoke("admission", "release", "--bucket", "morph", "--holder", "multica:fixture")
	if err := collect.Wait(); err != nil || collectOut.String() != "fixture answer" {
		t.Fatalf("delegate wait: %v %s", err, collectOut.String())
	}
	if err := await.Wait(); err != nil || awaitOut.String() != "fixture answer" {
		t.Fatalf("await: %v %s", err, awaitOut.String())
	}
	final := status()
	if final.State != model.StateCompleted || final.Admission.Phase != "granted" || final.DeadlineAt == nil ||
		final.Admission.GrantedAt == nil || final.DeadlineAt.Sub(*final.Admission.GrantedAt) != 5*time.Second || final.RateLimit != nil {
		t.Fatalf("final: %#v", final)
	}
	out := invoke("admission", "status")
	for _, want := range []string{`"in_use":0`, `"attempts":2`, `"throttled_attempts":1`, `"throttle_rate":0.5`} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %s: %s", want, out)
		}
	}
	prom := invoke("admission", "status", "--format", "prom")
	if !strings.Contains(prom, `agentctl_admission_throttled_attempts_total{bucket="morph",host=`) || strings.Contains(prom, "multica:fixture") || strings.Contains(prom, queued.ID.String()) {
		t.Fatalf("metrics: %s", prom)
	}
	if events := invoke("events", queued.ID.String()); strings.Contains(events, "private-provider-text") {
		t.Fatal(events)
	}
}

func TestProviderAdmissionPlanAndDisabledRun(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f, script := admissionFixture(t, enabled, "1s")
			code, plan := f.invoke("test prompt", "--plan")
			if code != 0 || strings.Contains(plan, `"phase":"planned"`) != enabled {
				t.Fatalf("plan: %d %s", code, plan)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(f.journal), "admission")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("plan wrote admission state: %v", err)
			}
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			if code := a.run(context.Background(), []string{"--config", f.config, "--journal", f.journal, "run", "--cwd", f.root, "--", script, "run", "--format", "json", "--model", "morph/morph-dsv41flash", "--", "native prompt"}); code != 0 {
				t.Fatalf("run: %d %s", code, stdout.String())
			}
			if strings.Contains(stdout.String(), `"phase":"granted"`) != enabled {
				t.Fatal(stdout.String())
			}
		})
	}
}

func TestProviderAdmissionOwnerCrashKeepsNativeLease(t *testing.T) {
	t.Parallel()
	f, script := admissionFixture(t, true, "5s")
	body := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'opencode 2.3.2'; exit 0; fi\nexec /bin/sleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "agentctl")
	if out, err := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner := exec.CommandContext(ctx, binary, "--config", f.config, "--journal", f.journal,
		"run", "--cwd", f.root, "--", script, "run", "--format", "json", "--model", "morph/morph-dsv41flash", "--", "task")
	owner.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off")
	var ownerOutput bytes.Buffer
	owner.Stdout, owner.Stderr = &ownerOutput, &ownerOutput
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if owner.ProcessState == nil {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	}()
	slots := admission.New(f.journal, time.Now, admissionProcessStart)
	b := admission.Bucket{Harness: "opencode", Model: "morph/morph-dsv41flash", MinWindow: 1, InitialWindow: 1, MaxWindow: 1}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var child admission.Owner
	for child.PID == 0 {
		view, err := slots.Status(ctx, "morph", b)
		if err == nil && len(view.Holders) == 1 && view.Holders[0].Child != nil {
			child = *view.Holders[0].Child
			break
		}
		select {
		case <-ctx.Done():
			_ = owner.Wait()
			t.Fatalf("native child lease was not recorded: %#v %v %s", view, err, &ownerOutput)
		case <-ticker.C:
		}
	}
	process, err := os.FindProcess(child.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if start, err := admissionProcessStart(child.PID); err == nil && start == child.Start {
			_ = process.Kill()
		}
	}()
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	if start, err := admissionProcessStart(child.PID); err != nil || start != child.Start {
		t.Fatalf("child did not survive owner crash: %q %v", start, err)
	}
	testPID := os.Getpid()
	testStart, err := admissionProcessStart(testPID)
	if err != nil || testStart == "" {
		t.Fatalf("contender identity: %q %v", testStart, err)
	}
	q := slots.Queue("morph", b, "contender", admission.Interactive, admission.Owner{PID: testPID, Start: testStart}, admission.Owner{}, false)
	defer q.Close()
	if d, err := q.Try(ctx, false); err != nil || d.Granted {
		t.Fatalf("owner crash released a live child's lease: %#v %v", d, err)
	}
}

func TestProviderAdmissionObservedAdjustment(t *testing.T) {
	f, script := admissionFixture(t, true, "5s")
	raw, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	buckets := cfg.Profiles["test"].Admission.Buckets
	bucket := buckets["morph"]
	bucket.InitialWindow, bucket.MaxWindow, bucket.IncreaseIntervalSeconds = 2, 4, 1
	buckets["morph"] = bucket
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte("echo launch"), []byte("sleep 1.25\necho launch"), 1)
	if err := os.WriteFile(script, body, 0700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	invoke := func(args ...string) string {
		t.Helper()
		stdout.Reset()
		if code := a.run(context.Background(), append([]string{"--config", f.config, "--journal", f.journal}, args...)); code != 0 {
			t.Fatalf("%v: %d %s", args, code, stdout.String())
		}
		return stdout.String()
	}
	out := invoke("run", "--cwd", f.root, "--", script, "run", "--format", "json", "--model=morph/morph-dsv41flash", "--", "native prompt")
	var doc struct{ Result model.Execution }
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if got := invoke("admission", "status", "--bucket", "morph"); !strings.Contains(got, `"window":1`) || !strings.Contains(got, `"reason":"above_target"`) {
		t.Fatal(got)
	}
	events := invoke("events", doc.Result.ID.String())
	if strings.Count(events, `"source_state":"admission.window_adjusted"`) != 1 || strings.Contains(events, "private-provider-text") {
		t.Fatal(events)
	}
}

func TestProviderAdmissionRunBudgetStartsAfterProbe(t *testing.T) {
	f, script := admissionFixture(t, true, "5s")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte("echo 'opencode 2.3.2'"), []byte("sleep 2\n  echo 'opencode 2.3.2'"), 1)
	if err := os.WriteFile(script, body, 0700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--config", f.config, "--journal", f.journal, "run",
		"--cwd", f.root, "--timeout", "1s", "--", script, "run", "--format", "json",
		"--model", "morph/morph-dsv41flash", "--", "native prompt"}); code != 0 {
		t.Fatalf("run: %d %s", code, stdout.String())
	}
	var doc struct{ Result model.Execution }
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	row := doc.Result
	if row.Admission == nil || row.Admission.Phase != "granted" || row.Admission.GrantedAt == nil ||
		row.DeadlineAt == nil || row.DeadlineAt.Sub(*row.Admission.GrantedAt) != time.Second {
		t.Fatalf("deadline was not assigned at admission: %s", stdout.String())
	}
}

func TestProviderAdmissionLongQueueRemainsCollectable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	now := time.Now().UTC()
	a.now = func() time.Time { return now }
	fresh := nativeRunnerLeaseSeconds
	queued := model.Execution{
		State: model.StateStarting, CreatedAt: now.Add(-61 * time.Second),
		Admission:   &model.ProviderAdmission{Phase: "queued", Bucket: "morph", Priority: admission.Background},
		Observation: model.Observation{ObservedAt: now, FreshForSeconds: &fresh},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	problem := a.collectDelegation(ctx, output.Renderer{Mode: output.JSON, Writer: &stdout}, common{}, queued, delegateOptions{wait: true}, false)
	if problem == nil || problem.Code != output.CodeTimeout {
		t.Fatalf("live queue past one minute must reach caller cancellation: %v", problem)
	}
}

func TestProviderAdmissionFallbackWarnings(t *testing.T) {
	for _, tc := range []struct{ name, wait, phase, warning string }{
		{"full", "500ms", "granted", "admission_max_wait"},
		{"unavailable", "30s", "bypassed", "admission_state_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, script := admissionFixture(t, true, tc.wait)
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			invoke := func(wantCode int, args ...string) string {
				t.Helper()
				stdout.Reset()
				code := a.run(ctx, append([]string{"--config", f.config, "--journal", f.journal}, args...))
				if code != wantCode {
					t.Fatalf("%v: exit %d want %d: %s", args, code, wantCode, stdout.String())
				}
				return stdout.String()
			}
			if tc.name == "full" {
				if got := invoke(0, "admission", "acquire", "--bucket", "morph", "--holder", "fixture", "--pid", strconv.Itoa(os.Getpid()), "--no-wait"); !strings.Contains(got, `"granted":true`) {
					t.Fatal(got)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(f.journal), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(filepath.Dir(f.journal), "admission"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			run := invoke(0, "run", "--cwd", f.root, "--timeout", "2s", "--", script, "run", "--format", "json", "--model", "morph/morph-dsv41flash", "--", "native prompt")
			var doc struct {
				Result   model.Execution
				Warnings []output.Warning
			}
			if err := json.Unmarshal([]byte(run), &doc); err != nil {
				t.Fatal(err)
			}
			row := doc.Result
			if row.State != model.StateCompleted || row.Admission == nil || row.Admission.Phase != tc.phase ||
				row.Admission.Warning != tc.warning || row.Admission.GrantedAt == nil || row.DeadlineAt == nil ||
				row.DeadlineAt.Sub(*row.Admission.GrantedAt) != 2*time.Second || len(doc.Warnings) == 0 || doc.Warnings[0].Code != tc.warning {
				t.Fatalf("missing fallback evidence: %s", run)
			}
			if events := invoke(0, "events", row.ID.String()); !strings.Contains(events, `"admission_warning":"`+tc.warning+`"`) {
				t.Fatal(events)
			}
			if tc.name == "full" {
				status := invoke(0, "admission", "status")
				if !strings.Contains(status, `"admitted":2`) || !strings.Contains(status, `"in_use":1`) {
					t.Fatalf("max-wait launch did not take and release its lease: %s", status)
				}
			}
		})
	}
}

func TestProviderAdmissionExternalCLI(t *testing.T) {
	f, _ := admissionFixture(t, true, "5s")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	invoke := func(wantCode int, want string, args ...string) {
		t.Helper()
		stdout.Reset()
		code := a.run(context.Background(), append([]string{"--config", f.config, "--journal", f.journal, "admission"}, args...))
		if code != wantCode || !strings.Contains(stdout.String(), want) {
			t.Fatalf("%v: exit %d want %d and %s: %s", args, code, wantCode, want, stdout.String())
		}
	}
	invoke(0, `"matched":true`, "match", "--harness", "opencode", "--model", "morph/morph-dsv41flash")
	invoke(0, `"matched":false`, "match", "--harness", "opencode", "--model", "another/model")
	invoke(2, `"admission_invalid_arguments"`, "observe", "--bucket", "morph", "--attempts", "1", "--throttled", "2")
	invoke(2, `"admission_invalid_arguments"`, "observe", "--bucket", "morph", "--attempts", "1", "--throttled", "0", "--at", "invalid")
	invoke(0, `"recorded":true`, "observe", "--bucket", "morph", "--attempts", "5", "--throttled", "1", "--at", time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano))
	invoke(0, `"throttle_rate":0.2`, "status", "--bucket", "morph")
	keyed := []string{"observe", "--bucket", "morph", "--attempts", "3", "--throttled", "1", "--idempotency-key"}
	invoke(2, `"admission_invalid_arguments"`, append(keyed, "")...)
	invoke(0, `"recorded":true`, append(keyed, "external:batch-1")...)
	invoke(0, `"duplicate":true`, append(keyed, "external:batch-1")...)
	changed := append([]string(nil), keyed...)
	changed[4] = "4"
	invoke(8, `"admission_observation_key_conflict"`, append(changed, "external:batch-1")...)
	invoke(0, `"total_attempts":8`, "status", "--bucket", "morph")
	pid := strconv.Itoa(os.Getpid())
	invoke(0, `"granted":true`, "acquire", "--bucket", "morph", "--holder", "fixture", "--pid", pid, "--no-wait")
	invoke(0, `"granted":false`, "acquire", "--bucket", "morph", "--holder", "another", "--pid", pid, "--no-wait")
	invoke(0, `"reason":"wait_timeout"`, "acquire", "--bucket", "morph", "--holder", "another", "--pid", pid, "--wait", "200ms")
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	invoke(8, `"admission_holder_conflict"`, "acquire", "--bucket", "morph", "--holder", "fixture", "--pid", strconv.Itoa(child.Process.Pid), "--no-wait")
	invoke(0, `"released":true`, "release", "--bucket", "morph", "--holder", "fixture")
	slots := admission.New(f.journal, time.Now, admissionProcessStart)
	cfg, err := config.Load(f.config)
	if err != nil {
		t.Fatal(err)
	}
	owner := admission.Owner{PID: os.Getpid(), Start: "native fixture"}
	queue := slots.Queue("morph", cfg.Profiles["test"].Admission.Buckets["morph"], "native-fixture", admission.Background, owner, owner, false)
	defer queue.Close()
	if decision, err := queue.Try(context.Background(), false); err != nil || !decision.Granted {
		t.Fatalf("native slot: %#v %v", decision, err)
	}
	invoke(8, `"admission_holder_conflict"`, "release", "--bucket", "morph", "--holder", "native-fixture")
}

func TestProviderAdmissionJournalUpdateRespectsContext(t *testing.T) {
	f, _ := admissionFixture(t, true, "5s")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	holder, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	c := common{journalPath: f.journal, journalBusyBudget: defaultJournalBusyBudget}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan *output.Error, 1)
	go func() {
		_, problem := a.updateProviderAdmission(ctx, c, model.Execution{}, model.ProviderAdmission{}, false, 0)
		done <- problem
	}()
	select {
	case problem := <-done:
		if problem == nil || ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("busy write returned before the configured deadline: %v", problem)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("busy journal pinned the admission writer past its deadline")
	}
}

func TestProviderAdmissionCrashGapRequiresNativeLaunchProof(t *testing.T) {
	start := time.Now().UTC()
	owner := &model.LaunchIdentity{PID: 42, StartedAt: start}
	row := model.Execution{State: model.StateStarting, OwnerLaunch: owner,
		Admission: &model.ProviderAdmission{Phase: "queued", Bucket: "morph", Priority: admission.Background}}
	if identity, ok := recordedLaunch(row); !ok || identity.PID != owner.PID {
		t.Fatalf("queued crash lost owner identity: %+v (%v)", identity, ok)
	}
	row.Admission.Phase = "granted"
	if identity, ok := recordedLaunch(row); ok {
		t.Fatalf("owner identity cannot prove a possibly launched child is gone: %+v", identity)
	}
	row.Launch = &model.LaunchIdentity{PID: 43, StartedAt: start}
	if identity, ok := recordedLaunch(row); !ok || identity.PID != row.Launch.PID {
		t.Fatalf("recorded child must take precedence: %+v (%v)", identity, ok)
	}
}
