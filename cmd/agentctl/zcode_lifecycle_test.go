package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestZCodeEventWriteFailureReapsAndTerminalizes(t *testing.T) {
	f := carryFixture(t, "zcode")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 0.17.0; exit 0; fi\nprintf '%s\\n' 'fixture starting' >&2\nsleep 60\n"
	if err := os.WriteFile(filepath.Join(f.root, "cursor-agent"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	var identity processIdentity
	a.nativeEventWriter = func(ctx context.Context, journal *store.Journal, e model.Execution, event adapter.Event) (model.Execution, error) {
		if e.Launch != nil {
			identity, _ = recordedLaunch(e)
		}
		event.Payload["usage"] = strings.Repeat("PRIVATE-TEXT", 7000)
		return appendNativeEvent(ctx, journal, e, event)
	}
	code := a.run(context.Background(), []string{"--config", f.config, "--journal", f.journal, "run", "--adapter", "zcode", "--cwd", f.root, "--", filepath.Join(f.root, "cursor-agent"), "--json", "--prompt", "fixture"})
	if code != output.ExitCodeFor(output.CodeExecutionUnknown) || !strings.Contains(stdout.String(), `"terminal_recorded":true`) || strings.Contains(stdout.String(), "PRIVATE-TEXT") || !strings.Contains(stdout.String(), `"journal_error_class":"event_validation"`) || !strings.Contains(stdout.String(), `"journal_operation":"commit_observed_event"`) {
		t.Fatalf("code=%d output=%s", code, stdout.String())
	}
	if identity.PID == 0 || !inspectProcess(identity).Gone {
		t.Fatalf("child %d not reaped", identity.PID)
	}
	journal, err := store.Open(f.journal, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	executions, err := journal.ListExecutions(context.Background(), false)
	if err != nil || len(executions) != 1 || !executions[0].State.Terminal() {
		t.Fatalf("records=%+v err=%v", executions, err)
	}
	outcome, err := journal.GetOutcome(context.Background(), executions[0].ID)
	if err != nil || outcome.Failure == nil || outcome.Failure.Code != "native_event_write_failed" || outcome.NativeExitCode == nil {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	events, err := journal.ListEvents(context.Background(), executions[0].ID, contracts.EventQuery{})
	if err != nil || events[len(events)-1].Kind != model.EventTerminal {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestZCodeInstantExitClassifiesFirstStderr(t *testing.T) {
	f := carryFixture(t, "zcode")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 0.17.0; exit 0; fi\nprintf '%s\\n' 'unknown option --private-secret' >&2\nexit 23\n"
	if err := os.WriteFile(filepath.Join(f.root, "cursor-agent"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	_, out := f.invoke("fixture", "--wait")
	if !strings.Contains(out, `"code":"invalid_arguments"`) || !strings.Contains(out, `"native_exit_code":23`) || !strings.Contains(out, `"stderr":`) || strings.Contains(out, "private-secret") {
		t.Fatal(out)
	}
}

func TestZCodePrelaunchRefusalClassified(t *testing.T) {
	for _, reason := range []string{"", "project_config_present"} {
		result := nativeLaunchFailure(&adapter.AdapterError{Code: adapter.ErrCapabilityUnavailable, Message: "zcode read_only profile refused: private directory", Details: map[string]any{"diagnostic_code": "zcode_read_only_unavailable", "reason_code": reason}}, adapter.StateFailed)
		e := model.Execution{Adapter: "zcode", State: model.StateFailed}
		outcome := buildOutcome(e, result, time.Now(), false)
		want := "zcode_read_only_unavailable"
		if reason != "" {
			want = "zcode_read_only_project_config_present"
		}
		if outcome.Failure == nil || outcome.Failure.Code != want || strings.Contains(outcome.Failure.Message, "private directory") || outcome.NativeExitCode != nil {
			t.Fatalf("%+v", outcome)
		}
	}
}

func TestZCodeOrphanRepairGoneAndRefusesAlive(t *testing.T) {
	for _, state := range []string{"gone", "alive", "unproven"} {
		t.Run(state, func(t *testing.T) {
			gone := state == "gone"
			world := seedReconcileWorld(t)
			journal, err := store.Open(world.path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			world.gone = mustCreateRunning(t, journal, "zcode", "zcode_session", "4242", world.now.Add(-48*time.Hour), nil)
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			a.now = func() time.Time { return world.now }
			a.processProof = func(id processIdentity) processProof {
				return processProof{Gone: gone, Alive: state == "alive", Present: !gone, Proof: map[string]string{"gone": "pid_absent", "alive": "pid_alive", "unproven": "start_unproven"}[state]}
			}
			code := a.run(context.Background(), []string{"--journal", world.path, "cancel", world.gone.ID.String(), "--orphan-if-gone"})
			if gone {
				if code != 0 || !strings.Contains(stdout.String(), `"state":"orphaned"`) || !strings.Contains(stdout.String(), "owner_lost") {
					t.Fatalf("code=%d %s", code, stdout.String())
				}
			} else {
				if code != output.ExitCodeFor(output.CodeInvalidState) {
					t.Fatalf("code=%d %s", code, stdout.String())
				}
				assertUntouched(t, world.path, world.gone)
			}
		})
	}
}
