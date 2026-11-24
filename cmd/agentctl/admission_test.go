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
	// Hold the external reservation beyond the previous 60-second unknown
	// cutoff and well beyond the new run's five-second execution budget.
	until := queued.CreatedAt.Add(62 * time.Second)
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for time.Now().Before(until) {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-poll.C:
		}
	}
	current := status()
	if !current.ProviderQueued() || current.Admission.Position != 1 || current.DeadlineAt != nil || time.Since(current.CreatedAt) <= time.Minute {
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
