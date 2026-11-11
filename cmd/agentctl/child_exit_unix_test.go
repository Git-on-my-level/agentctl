//go:build darwin || linux

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
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestChildExitThroughBuiltBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "agentctl")
	if out, err := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, variant := range []string{"resume_once", "disabled", "without_children", "failed_turn", "timeout", "opencode", "opencode_unverified", "codex_failed"} {
		t.Run(variant, func(t *testing.T) {
			f := newDelegateFixture(t, "")
			if strings.HasPrefix(variant, "opencode") || variant == "codex_failed" {
				harness := "opencode"
				if variant == "codex_failed" {
					harness = "codex"
				}
				cfg := config.Config{SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{"test": {Adapters: map[string]config.Adapter{harness: {Executable: filepath.Join(f.root, "cursor-agent")}}, AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: harness, Model: "fixture/model", Family: "test"}}}}}}
				raw, _ := json.Marshal(cfg)
				if err := os.WriteFile(f.config, raw, 0600); err != nil {
					t.Fatal(err)
				}
				f.selectRequest(t, `{"family":"test"}`)
			}
			script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 2026.09.10; exit 0; fi\necho launch >> " + shellQuote(f.count) + "\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"chat-1\"}'\n"
			if variant == "codex_failed" {
				script += `printf '%s\n' '{"type":"thread.started","thread_id":"thread-fixture"}'` + "\n"
			}
			if variant != "without_children" {
				script += remoteCommand(binary, "--journal", f.journal, "run", "--adapter", "generic-process", "--", "/bin/sh", "-c", `printf '%s\n' '{"type":"result","status":"completed","result":"child answer"}'`) + " > " + shellQuote(filepath.Join(f.root, "child-receipt")) + "\n"
			}
			if strings.HasPrefix(variant, "opencode") {
				version := "2.2.6"
				if variant == "opencode_unverified" {
					version = "2.2.7"
				}
				script = strings.Replace(script, "echo 2026.09.10", "echo 'opencode "+version+"'", 1)
				// Session existence verification is a read, not a model turn.
				script = strings.Replace(script, "echo launch >>", "if [ \"$1\" = session ]; then exit 0; fi\necho launch >>", 1)
			}
			if variant == "timeout" {
				script += "sleep 2\n"
			}
			if variant == "codex_failed" {
				script += `case " $* " in
*" exec resume "*) printf '%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"resumed answer"}}' '{"type":"turn.completed"}' ;;
*) printf '%s\n' '{"type":"turn.failed","error":{"message":"invalid task"}}'; exit 1 ;;
esac` + "\n"
			} else if variant == "failed_turn" {
				script += "printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"result\":\"parent stopped\"}'\nexit 1\n"
			} else if strings.HasPrefix(variant, "opencode") {
				script += `printf '%s\n' '{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}' '{"type":"text","sessionID":"s1","part":{"type":"text","text":"parent answer","time":{"end":1}}}' '{"type":"step_finish","sessionID":"s1","part":{"type":"step-finish","reason":"stop"}}'` + "\n"
			} else {
				script += continueAgent + "\n"
			}
			if err := os.WriteFile(filepath.Join(f.root, "cursor-agent"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if variant == "disabled" {
				cfg, _ := config.Load(f.config)
				profile := cfg.Profiles["test"]
				disabled := false
				profile.Delegation = &config.DelegationPolicy{ChildExitAutoContinue: &disabled}
				cfg.Profiles["test"] = profile
				raw, _ := json.Marshal(cfg)
				if err := os.WriteFile(f.config, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin"}
			if variant == "timeout" {
				args = append(args, "--timeout", "700ms")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, args...)
			command.Stdin = strings.NewReader("parent task")
			command.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off")
			out, err := command.CombinedOutput()
			if err != nil && variant != "failed_turn" && variant != "timeout" {
				t.Fatalf("parent: %v %s", err, out)
			}
			j, err := store.Open(f.journal, store.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			executions, err := j.ListExecutions(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			var parent model.Execution
			var auto []model.Execution
			for _, execution := range executions {
				if execution.Delegation != nil && execution.AutoContinuationOf == nil && len(execution.Supersedes) == 0 {
					parent = execution
				}
				if execution.AutoContinuationOf != nil {
					auto = append(auto, execution)
				}
			}
			if parent.ID.IsZero() {
				t.Fatalf("missing parent: %+v %s", executions, out)
			}
			wantLaunches := 1
			if variant == "resume_once" || variant == "codex_failed" || variant == "opencode" {
				wantLaunches = 2
			}
			if f.launches(t) != wantLaunches {
				t.Fatalf("%d launches want %d; output=%s", f.launches(t), wantLaunches, out)
			}
			if variant == "without_children" {
				if parent.ChildExitGuard != nil {
					t.Fatal("childless run changed")
				}
				return
			}
			if parent.ChildExitGuard == nil || len(parent.UncollectedChildren) != 1 {
				t.Fatalf("parent guard=%+v children=%+v output=%s", parent.ChildExitGuard, parent.UncollectedChildren, out)
			}
			outcome, err := j.GetOutcome(context.Background(), parent.ID)
			if err != nil || len(outcome.UncollectedChildren) != 1 {
				t.Fatalf("outcome=%+v err=%v", outcome, err)
			}
			if variant == "resume_once" || variant == "codex_failed" || variant == "opencode" {
				if len(auto) != 1 || auto[0].ChildExitGuard == nil || auto[0].ChildExitGuard.AutoContinue != "suppressed_recursive" {
					t.Fatalf("automatic lineage=%+v", auto)
				}
				if !bytes.Contains(out, []byte(`"auto_continue":"launched"`)) {
					t.Fatalf("missing warning: %s", out)
				}
			} else if len(auto) != 0 {
				t.Fatalf("unexpected continuation: %+v", auto)
			}
		})
	}
}
