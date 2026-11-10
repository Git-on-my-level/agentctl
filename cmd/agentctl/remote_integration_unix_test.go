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
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// Execute the generated SSH shell commands against an isolated real agentctl.
// This catches protocol, quoting, remote-local host resolution, and detached
// continuation bugs that canned receipts cannot detect. No network is used.
func TestRemoteThroughBuiltBinary(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "agentctl")
	if out, err := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	f := newDelegateFixture(t, delegateSuccessScript)
	f.selectRequest(t, `{"family":"grok","host":"remote"}`)
	cfg, err := config.Load(f.config)
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.Profiles["test"]
	profile.Route.ThisHost = "other-workstation"
	cfg.Profiles["test"] = profile
	body, _ := json.Marshal(cfg)
	remoteConfig := filepath.Join(dir, "remote-config.json")
	if err := os.WriteFile(remoteConfig, body, 0600); err != nil {
		t.Fatal(err)
	}
	remoteState := filepath.Join(dir, "remote-state")
	staged := filepath.Join(dir, "remote-tmp")
	if err := os.Mkdir(staged, 0700); err != nil {
		t.Fatal(err)
	}
	ssh := filepath.Join(dir, "fake-ssh")
	script := "#!/bin/sh\nset -eu\nexport AGENTCTL_CONFIG=" + shellQuote(remoteConfig) + "\nexport AGENTCTL_STATE_HOME=" + shellQuote(remoteState) + "\nexport AGENTCTL_UPDATE_MODE=off\nexport TMPDIR=" + shellQuote(staged) + "\nexport PATH=" + shellQuote(dir) + ":\"$PATH\"\nwhile [ \"$1\" = -o ]; do shift 2; done\nexec /bin/sh -c \"$3\"\n"
	gate := filepath.Join(dir, "collect-gate")
	collecting := filepath.Join(dir, "collecting")
	allow := filepath.Join(dir, "collect-allow")
	hook := "if [ -f " + shellQuote(gate) + " ]; then case \"$3\" in *\"'await'\"*) touch " + shellQuote(collecting) + "; n=0; while [ ! -f " + shellQuote(allow) + " ] && [ \"$n\" -lt 1000 ]; do sleep 0.01; n=$((n+1)); done ;; esac; fi\n"
	script = strings.Replace(script, "exec /bin/sh -c \"$3\"\n", hook+"exec /bin/sh -c \"$3\"\n", 1)
	if err := os.WriteFile(ssh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_SSH", ssh)
	invoke := func(prompt string, args ...string) (int, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		argv := append([]string{"--config", f.config, "--journal", f.journal}, args...)
		cmd := exec.CommandContext(ctx, binary, argv...)
		cmd.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off")
		cmd.Stdin = strings.NewReader(prompt)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatalf("run: %v %s", err, stderr.String())
			}
		}
		return code, stdout.String()
	}
	// --wait collects the detached remote run's result through real wire shapes.
	args := []string{"delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--timeout", "5s", "--wait"}
	code, out := invoke("private task ' $(false)\n", args...)
	if code != 0 || !strings.Contains(out, "verified answer") {
		t.Fatalf("delegate: %d %s", code, out)
	}
	if entries, err := os.ReadDir(staged); err != nil || len(entries) != 0 {
		t.Fatalf("remote launch left staging files: %v %v", entries, err)
	}
	local := remoteLocalID(t, out)
	code, out = invoke("follow-up ' $(false)\n", "continue", local, "--request-key", "next-real-turn", "--prompt-stdin", "--timeout", "5s", "--wait")
	if code != 0 || !strings.Contains(out, "verified answer") || !strings.Contains(out, `"continues":"`+local+`"`) {
		t.Fatalf("continue: %d %s", code, out)
	}
	next := remoteLocalID(t, out)
	if next == local {
		t.Fatal("continuation reused source id")
	}
	code, out = invoke("", "await", next, "--content", "--timeout", "3s")
	if code != 0 || out != "verified answer" {
		t.Fatalf("await: %d %s", code, out)
	}
	launches := string(mustRead(t, f.count))
	if strings.Count(launches, "launch") != 2 {
		t.Fatalf("launches: %s", launches)
	}
	// A real remote CLI must retain its busy budget after status already
	// proved terminal. No-timeout must not invent a shorter collection deadline.
	for _, item := range []struct {
		key   string
		hold  time.Duration
		flags []string
	}{
		{"bounded", 700 * time.Millisecond, []string{"--timeout", "3s"}},
		{"unbounded", 11 * time.Second, []string{"--no-timeout"}},
	} {
		request := `{"schema_version":1,"request_key":"` + item.key + `","selector":{"family":"grok","host":"remote"}}`
		if err := os.WriteFile(f.request, []byte(request), 0600); err != nil {
			t.Fatal(err)
		}
		code, out = invoke("task", "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--label", "noise."+item.key)
		if code != 0 {
			t.Fatal(out)
		}
		id := remoteLocalID(t, out)
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
		held := make(chan error, 1)
		go func() {
			until := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(collecting); err == nil {
					break
				}
				if time.Now().After(until) {
					held <- context.DeadlineExceeded
					return
				}
				time.Sleep(time.Millisecond)
			}
			holder, err := store.Open(filepath.Join(remoteState, "journal.db"), store.Options{LockTimeout: time.Second})
			if err != nil {
				held <- err
				return
			}
			if err := os.WriteFile(allow, nil, 0600); err != nil {
				holder.Close()
				held <- err
				return
			}
			time.Sleep(item.hold)
			held <- holder.Close()
		}()
		args := append([]string{"await-any", "--execution", id}, item.flags...)
		code, out = invoke("", args...)
		err := <-held
		for _, path := range []string{gate, collecting, allow} {
			_ = os.Remove(path)
		}
		if err != nil || code != 0 || !strings.Contains(out, `"execution_id":"`+id+`"`) {
			t.Fatalf("%s collection: held=%v exit=%d %s", item.key, err, code, out)
		}
	}
	// Exercise actual remote lock contention, rather than just inspecting argv.
	if err := os.WriteFile(f.request, []byte(`{"schema_version":1,"request_key":"remote-busy","selector":{"family":"grok","host":"remote"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	holder, err := store.Open(filepath.Join(remoteState, "journal.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	for _, fromEnv := range []bool{false, true} {
		budgetArgs := []string{"delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--plan"}
		if fromEnv {
			t.Setenv(journalBusyBudgetEnv, "70ms")
		} else {
			budgetArgs = append(budgetArgs, "--journal-busy-budget", "70ms")
		}
		code, out = invoke("task", budgetArgs...)
		if code != 6 || !strings.Contains(out, `"busy_budget_ms":70`) || !strings.Contains(out, `"diagnostic_code":"journal_busy"`) {
			t.Fatalf("remote budget (env=%v): %d %s", fromEnv, code, out)
		}
	}
	holder.Close()
	// Both detached owners ran with deadlines and were collected terminal before
	// the fixture is removed. No servers, supervisors, or remote agents persist.
}
