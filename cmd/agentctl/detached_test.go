package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestDetachedPlanAndFlagConflicts(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	code, out := f.invoke("private-detached-plan", "--detach", "--plan")
	if code != 0 || !strings.Contains(out, `"owner":"detached_process"`) || !strings.Contains(out, `"reboot_recovery":false`) || strings.Contains(out, "private-detached-plan") {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(f.journal); !os.IsNotExist(err) {
		t.Fatalf("plan created journal: %v", err)
	}
	if _, err := os.Stat(f.count); !os.IsNotExist(err) {
		t.Fatalf("plan launched: %v", err)
	}
	code, out = f.invoke("private", "--detach", "--authority", "multica")
	if code != 5 || !strings.Contains(out, "delegate_multica_result_unavailable") {
		t.Fatalf("Multica boundary: %d %s", code, out)
	}
	for _, flags := range [][]string{{"--detach", "--wait"}, {"--detach", "--wait", "--content"}, {"--detach", "--detach"}, {"--detach", "--content"}} {
		code, out := f.invoke("private", flags...)
		if code != 2 {
			t.Fatalf("%v: %d %s", flags, code, out)
		}
	}
}

func TestAwaitContentDeliveryAndCollection(t *testing.T) {
	for _, scenario := range []string{"stored", "missing", "multica_missing", "delivery_failure", "conflict", "failed", "cancelled", "orphaned", "attention"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDelegateFixture(t, delegateSuccessScript)
			code, out := f.invoke("private", "--label", "collection")
			if code != 0 {
				t.Fatalf("%d %s", code, out)
			}
			var doc struct {
				Result model.Execution `json:"result"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatal(err)
			}
			journal, err := store.Open(f.journal, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			execution, err := journal.GetExecution(context.Background(), doc.Result.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "multica_missing":
				execution.Authority = model.AuthorityMultica
				execution.Mode = model.ModeMultica
				execution.Adapter = "multica"
			case "conflict":
				execution.Observation.Integrity = model.IntegrityConflicted
			case "failed":
				execution.State = model.StateFailed
			case "cancelled":
				execution.State = model.StateCancelled
			case "orphaned":
				execution.State = model.StateOrphaned
			case "attention":
				execution.State = model.StateAttention
				execution.TerminalAt = nil
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "conflict" {
				execution, err = journal.UpdateExecution(context.Background(), execution, execution.Revision)
			} else if scenario != "stored" && scenario != "delivery_failure" {
				execution.ID = model.Execution{}.ID
				execution.Delegation = nil
				execution.SourceBindings = nil
				execution, _, err = journal.CreateExecution(context.Background(), execution, contracts.MutationKey{})
			}
			if err != nil {
				t.Fatal(err)
			}
			journal.Close()
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			if scenario == "delivery_failure" {
				a.stdout = failingWriter{}
			}
			code = a.run(context.Background(), []string{"--journal", f.journal, "await", execution.ID.String(), "--content", "--timeout", "1s"})
			want := map[string]int{"stored": 0, "missing": 3, "multica_missing": 3, "delivery_failure": 70, "conflict": 13, "failed": 11, "cancelled": 12, "orphaned": 13, "attention": 10}[scenario]
			if code != want {
				t.Fatalf("got %d want %d: %s", code, want, stdout.String())
			}
			if scenario == "multica_missing" && !strings.Contains(stdout.String(), "multica_result_route_unavailable") {
				t.Fatal(stdout.String())
			}
			if scenario == "stored" && stdout.String() != "verified answer" {
				t.Fatal(stdout.String())
			}
			journal, err = store.Open(f.journal, store.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			ack, ackErr := journal.GetAcknowledgement(context.Background(), execution.ID)
			journal.Close()
			wantAck := scenario == "stored" || scenario == "failed" || scenario == "cancelled" || scenario == "orphaned"
			if (ackErr == nil) != wantAck {
				t.Fatalf("ack=%v error=%v want=%v", ack, ackErr, wantAck)
			}
		})
	}
}

func TestAwaitContentTimeoutLeavesExecutionUncollected(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	code, out := f.invoke("private")
	if code != 0 {
		t.Fatal(out)
	}
	var doc struct {
		Result model.Execution `json:"result"`
	}
	json.Unmarshal([]byte(out), &doc)
	journal, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	execution, _ := journal.GetExecution(context.Background(), doc.Result.ID)
	execution.State = model.StateRunning
	execution.TerminalAt = nil
	execution.ID = model.Execution{}.ID
	execution.Delegation = nil
	execution.SourceBindings = nil
	execution, _, err = journal.CreateExecution(context.Background(), execution, contracts.MutationKey{})
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	started := time.Now()
	code = a.run(context.Background(), []string{"--journal", f.journal, "await", execution.ID.String(), "--content", "--timeout", "1ms"})
	if code != 9 || time.Since(started) > 2*time.Second {
		t.Fatalf("%d %s", code, stdout.String())
	}
}
