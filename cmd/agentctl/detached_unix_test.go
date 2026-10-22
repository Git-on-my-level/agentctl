//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

// Journal contention is an explicit retryable outcome, including during a
// lost-receipt recovery while the owner commits its result. Exercise identical
// keyed retries rather than bypassing the journal or inventing a new task.
func retryLifecycleJournalBusy(invoke func() (int, string)) (int, string) {
	code, out := invoke()
	for attempt := 0; attempt < 4 && code == 6 && strings.Contains(out, `"diagnostic_code":"journal_busy"`); attempt++ {
		time.Sleep(time.Duration(20<<attempt) * time.Millisecond)
		code, out = invoke()
	}
	return code, out
}

func TestDetachedLifecycleThroughBuiltBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "agentctl")
	if out, err := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, harness := range []string{"cursor", "zcode", "claude-code", "codex", "omp"} {
		t.Run(harness, func(t *testing.T) {
			f := newDelegateFixture(t, "")
			release := filepath.Join(f.root, "release")
			result := delegateSuccessScript
			nativeModel := "cursor-grok-4.6-high"
			if harness == "zcode" {
				nativeModel = "zai/glm-5.3"
				result = `printf '%s\n' '{"sessionId":"sess_fixture","turnId":"turn_fixture","response":"verified answer","projection":{"status":"idle"}}'`
			}
			if harness == "claude-code" {
				nativeModel = "test-claude"
			}
			if harness == "codex" {
				nativeModel = "gpt-test"
				result = `printf '%s\n' '{"type":"thread.started","thread_id":"thread-fixture"}' '{"type":"item.completed","item":{"id":"item-1","type":"agent_message","text":"verified answer"}}' '{"type":"turn.completed"}'`
			}
			if harness == "omp" {
				nativeModel = "fixture/model"
				result = `printf '%s\n' '{"type":"session","version":3,"id":"omp-fixture"}' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"verified answer"}],"stopReason":"stop"}}' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"verified answer"}],"stopReason":"stop"}]}'`
			}

			cfg := config.Config{SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{"test": {
				Adapters:         map[string]config.Adapter{harness: {Executable: filepath.Join(f.root, "cursor-agent")}},
				AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: harness, Family: "test", Model: nativeModel}}},
			}}}
			data, _ := json.Marshal(cfg)
			if err := os.WriteFile(f.config, data, 0600); err != nil {
				t.Fatal(err)
			}
			f.selectRequest(t, `{"family":"test"}`)
			// The executable represents an explicitly configured account wrapper. Its
			// name need not match the harness, and account environment must survive.
			script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 2026.09.10; exit 0; fi\necho launch >> '" + f.count + "'\n[ \"$TEST_LIFECYCLE_ACCOUNT\" = second ] || exit 17\nwhile [ ! -f '" + release + "' ]; do sleep 0.05; done\n" + result + "\n"
			if err := os.WriteFile(filepath.Join(f.root, "cursor-agent"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			scratch := filepath.Join(f.root, "scratch")
			os.Mkdir(scratch, 0700)
			promptFile := filepath.Join(scratch, "task.md")
			if err := os.WriteFile(promptFile, []byte("private-detached-task"), 0600); err != nil {
				t.Fatal(err)
			}
			receipt := filepath.Join(f.root, "receipt.json")
			args := []string{"-c", `"$@" > "$TEST_LIFECYCLE_RECEIPT" && sleep 30`, "parent", binary, "--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-file", promptFile, "--detach", "--timeout", "10s"}
			parent := exec.Command("/bin/sh", args...)
			parent.Env = append(os.Environ(), "TEST_LIFECYCLE_RECEIPT="+receipt, "TEST_LIFECYCLE_ACCOUNT=second", "AGENTCTL_UPDATE_MODE=off")
			parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := parent.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = syscall.Kill(-parent.Process.Pid, syscall.SIGKILL); _ = parent.Wait() }()
			t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
			var doc struct {
				OK     bool            `json:"ok"`
				Result model.Execution `json:"result"`
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				body, _ := os.ReadFile(receipt)
				if json.Unmarshal(body, &doc) == nil && doc.OK && !doc.Result.ID.IsZero() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("receipt missing: %s", body)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if doc.Result.State.Terminal() {
				t.Fatalf("receipt claimed completion: %+v", doc.Result)
			}
			// Kill the caller's entire process group, then wipe only the prompt source.
			if err := syscall.Kill(-parent.Process.Pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(scratch); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for i := 0; i < 3; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					code, out := retryLifecycleJournalBusy(func() (int, string) { return f.invoke("private-detached-task", "--detach", "--timeout", "10s") })
					if code != 0 || !strings.Contains(out, doc.Result.ID.String()) || !strings.Contains(out, `"reused":true`) {
						t.Errorf("replay %d %s", code, out)
					}
				}()
			}
			wg.Wait()
			// Observer timeout does not cancel the owner or acknowledge collection.
			short := exec.Command(binary, "--journal", f.journal, "await", doc.Result.ID.String(), "--content", "--timeout", "1ms")
			if out, err := short.CombinedOutput(); err == nil || !bytes.Contains(out, []byte(`"exit_code":9`)) {
				t.Fatalf("observer timeout %v %s", err, out)
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			await := exec.CommandContext(ctx, binary, "--journal", f.journal, "await", doc.Result.ID.String(), "--content", "--timeout", "8s")
			await.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off")
			if out, err := await.CombinedOutput(); err != nil || string(out) != "verified answer" {
				t.Fatalf("await %v %s", err, out)
			}
			launches, _ := os.ReadFile(f.count)
			if strings.Count(string(launches), "launch") != 1 {
				t.Fatalf("replay launched again: %s", launches)
			}
			journal, _ := os.ReadFile(f.journal)
			if bytes.Contains(journal, []byte("private-detached-task")) {
				t.Fatal("prompt leaked into journal")
			}
		})
	}

	t.Run("devin_acp", func(t *testing.T) {
		f := newDevinACPDelegateFixture(t)
		launch := exec.Command(binary, "--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--detach", "--timeout", "5s")
		launch.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off", "ACP_FIXTURE_MODE=normal")
		launch.Stdin = strings.NewReader("private-acp-detached-prompt")
		out, err := launch.CombinedOutput()
		if err != nil {
			t.Fatalf("ACP admission: %v %s", err, out)
		}
		id := resultID(t, string(out))
		await := exec.Command(binary, "--journal", f.journal, "await", id, "--content", "--timeout", "6s")
		await.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off")
		if out, err := await.CombinedOutput(); err != nil || string(out) != "Hello world" {
			t.Fatalf("ACP result: %v %s", err, out)
		}
		journal, err := os.ReadFile(f.journal)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(journal, []byte("private-acp-detached-prompt")) {
			t.Fatal("ACP prompt leaked")
		}
	})

	t.Run("lost_receipt_before_launch", func(t *testing.T) {
		f := newDelegateFixture(t, delegateSuccessScript)
		ready := filepath.Join(f.root, "probe-ready")
		release := filepath.Join(f.root, "probe-release")
		script := "#!/bin/sh\nif [ \"$1\" = --version ]; then touch '" + ready + "'; while [ ! -f '" + release + "' ]; do sleep 0.05; done; echo 2026.09.10; exit 0; fi\necho launch >> '" + f.count + "'\n" + delegateSuccessScript + "\n"
		os.WriteFile(filepath.Join(f.root, "cursor-agent"), []byte(script), 0700)
		parent := exec.Command(binary, "--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--detach", "--timeout", "10s")
		parent.Stdin = strings.NewReader("lost-receipt-task")
		parent.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off")
		pipe, err := parent.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := parent.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = parent.Process.Kill(); _ = parent.Wait(); _ = os.WriteFile(release, nil, 0600) }()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("worker never reached probe")
			}
			time.Sleep(20 * time.Millisecond)
		}
		pipe.Close()
		parent.Process.Kill()
		os.WriteFile(release, nil, 0600)
		// With SIGPIPE ignored, the orphaned owner completes after receipt delivery
		// fails. Same-key replay recovers it and never starts another native child.
		for {
			if data, _ := os.ReadFile(f.count); len(data) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("lost receipt killed worker")
			}
			time.Sleep(20 * time.Millisecond)
		}
		code, out := retryLifecycleJournalBusy(func() (int, string) { return f.invoke("lost-receipt-task", "--wait", "--content", "--timeout", "10s") })
		if code != 0 || out != "verified answer" {
			t.Fatalf("recovered %d %s", code, out)
		}
		data, _ := os.ReadFile(f.count)
		if strings.Count(string(data), "launch") != 1 {
			t.Fatalf("duplicate launch: %s", data)
		}
	})
}

func TestDetachedWorkerConcurrentAdmissionAndFailure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "agentctl")
	if out, err := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, scenario := range []string{"concurrent", "failed", "deadline", "preflight_failure"} {
		t.Run(scenario, func(t *testing.T) {
			body := delegateSuccessScript
			timeout := "5s"
			if scenario == "failed" {
				body = "exit 19"
			}
			if scenario == "deadline" {
				body = "sleep 30"
				timeout = "1s"
			}
			f := newDelegateFixture(t, body)
			if scenario == "preflight_failure" {
				os.WriteFile(filepath.Join(f.root, "cursor-agent"), []byte("#!/bin/sh\nexit 19\n"), 0700)
			}
			args := []string{"--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--detach", "--timeout", timeout}
			var outputs [3][]byte
			var errs [3]error
			count := 1
			if scenario == "concurrent" {
				count = 3
			}
			var wg sync.WaitGroup
			for i := 0; i < count; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					cmd := exec.Command(binary, args...)
					cmd.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off")
					cmd.Stdin = strings.NewReader("private-concurrent-task")
					outputs[i], errs[i] = cmd.CombinedOutput()
				}(i)
			}
			wg.Wait()
			if scenario == "preflight_failure" {
				if errs[0] == nil || bytes.Contains(outputs[0], []byte("detached_receipt_unknown")) {
					t.Fatalf("preflight %v %s", errs[0], outputs[0])
				}
				if _, err := os.Stat(f.journal); !os.IsNotExist(err) {
					t.Fatalf("preflight created journal: %v", err)
				}
				return
			}
			id := ""
			for i := 0; i < count; i++ {
				var doc struct {
					Result model.Execution `json:"result"`
				}
				if errs[i] != nil || json.Unmarshal(outputs[i], &doc) != nil || doc.Result.ID.IsZero() {
					t.Fatalf("admission %v %s", errs[i], outputs[i])
				}
				if id != "" && id != doc.Result.ID.String() {
					t.Fatal("concurrent callers returned different executions")
				}
				id = doc.Result.ID.String()
			}
			cmd := exec.Command(binary, "--journal", f.journal, "await", id, "--content", "--timeout", "6s")
			cmd.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off")
			out, err := cmd.CombinedOutput()
			switch scenario {
			case "concurrent":
				if err != nil || string(out) != "verified answer" {
					t.Fatalf("%v %s", err, out)
				}
			case "failed":
				if err == nil || !bytes.Contains(out, []byte(`"exit_code":11`)) {
					t.Fatalf("%v %s", err, out)
				}
			case "deadline":
				if err == nil || !bytes.Contains(out, []byte(`"exit_code":12`)) {
					t.Fatalf("%v %s", err, out)
				}
			}
			data, _ := os.ReadFile(f.count)
			if strings.Count(string(data), "launch") != 1 {
				t.Fatalf("launches=%s", data)
			}
		})
	}
}
