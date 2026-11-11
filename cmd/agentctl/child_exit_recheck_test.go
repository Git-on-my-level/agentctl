package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// Reproduce the parent-exit race with a real process-held waiter lock. Neither
// the fresh child nor the acknowledged parent has any unrelated inbox reason.
func TestChildExitCoveredChildRecheckedAfterWaiterExit(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("kernel waiter evidence")
	}
	f := newDelegateFixture(t, continueAgent)
	ctx := context.Background()
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parent := identityFixture(t, j, "codex", "covered-parent")
	child := addExitChild(t, j, parent, false)
	waiterCtx, stopWaiter := context.WithTimeout(ctx, 10*time.Second)
	defer stopWaiter()
	process := exec.CommandContext(waiterCtx, os.Args[0], "-test.run=^TestChildExitWaiterProcessHelper$")
	process.Env = append(os.Environ(), "AGENTCTL_TEST_EXIT_WAITER=1", "AGENTCTL_TEST_EXIT_WAITER_JOURNAL="+f.journal, "AGENTCTL_TEST_EXIT_WAITER_ID="+child.ID.String())
	pipe, err := process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.Process.Kill(); _ = process.Wait() }()
	ready, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatalf("waiter helper: %q %v", ready, err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	c := common{configPath: f.config, journalPath: f.journal}
	parent, err = a.finalizeResult(ctx, c, j, parent, adapter.Result{Success: true, State: adapter.StateCompleted, Content: "parent answer"}, time.Now().UTC(), false)
	if err != nil {
		t.Fatal(err)
	}
	if parent.ChildExitGuard != nil || len(parent.UncollectedChildren) != 0 || len(childExitWarnings(parent)) != 0 {
		t.Fatalf("covered child falsely actionable: %+v", parent)
	}
	if _, _, err = j.AcknowledgeExecution(ctx, parent.ID, store.AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	j.Close()
	inboxParent := func() *inboxExecution {
		t.Helper()
		code, raw := f.command(t, "", "inbox")
		if code != 0 {
			t.Fatalf("inbox %d: %s", code, raw)
		}
		var envelope struct {
			Result struct {
				Executions []inboxExecution `json:"executions"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatal(err)
		}
		for _, item := range envelope.Result.Executions {
			if item.ID == parent.ID {
				return &item
			}
		}
		return nil
	}
	if item := inboxParent(); item != nil {
		t.Fatalf("covered parent falsely actionable: %+v", item)
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = process.Wait() // SIGKILL releases the live lock but leaves crash residue.
	item := inboxParent()
	if item == nil || len(item.UncollectedChildren) != 1 || item.UncollectedChildren[0].ID != child.ID || item.UncollectedChildren[0].State != model.StateRunning {
		t.Fatalf("parent lost child after waiter exit: %+v", item)
	}
	waiter, err := store.RegisterWaiter(ctx, f.journal, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	if item := inboxParent(); item != nil {
		t.Fatalf("new waiter did not cover child: %+v", item)
	}
	// Incomplete coverage must remain actionable, without inventing an
	// uncollected snapshot. Removing the excess residue restores covered status.
	var residue []string
	for n := 0; n < 129; n++ {
		path := filepath.Join(f.journal+".waiters", child.ID.String(), fmt.Sprintf("residue-%03d", n))
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		residue = append(residue, path)
	}
	if item := inboxParent(); item == nil || item.ChildExitGuard == nil || item.ChildExitGuard.CheckError != "children_query_incomplete" || len(item.UncollectedChildren) != 0 {
		t.Fatalf("incomplete recheck hidden or fabricated children: %+v", item)
	}
	for _, path := range residue {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if item := inboxParent(); item != nil {
		t.Fatalf("recovered covered recheck still actionable: %+v", item)
	}
	waiter.Close()
	j, err = store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	child, err = a.finalizeResult(ctx, c, j, child, adapter.Result{Success: true, State: adapter.StateCompleted, Content: "child answer"}, time.Now().UTC(), false)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	if item = inboxParent(); item == nil || item.UncollectedChildren[0].State != model.StateCompleted {
		t.Fatalf("terminal child hidden without collection: %+v", item)
	}
	j, err = store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = j.AcknowledgeExecution(ctx, child.ID, store.AcknowledgementAwait); err != nil {
		t.Fatal(err)
	}
	historical, err := j.GetOutcome(ctx, parent.ID)
	j.Close()
	if err != nil || !historical.ChildExitRecheck || !parent.ChildExitRecheck || len(historical.UncollectedChildren) != 0 {
		t.Fatalf("historical outcome changed: %+v %v", historical, err)
	}
	if item = inboxParent(); item != nil {
		t.Fatalf("collected child remains actionable: %+v", item)
	}
}

// Test-only collector: isolated metadata, no adapter launch or delivery CLI.
func TestChildExitWaiterProcessHelper(t *testing.T) {
	if os.Getenv("AGENTCTL_TEST_EXIT_WAITER") != "1" {
		return
	}
	id, err := ids.ParseExecutionID(os.Getenv("AGENTCTL_TEST_EXIT_WAITER_ID"))
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := store.RegisterWaiter(context.Background(), os.Getenv("AGENTCTL_TEST_EXIT_WAITER_JOURNAL"), id)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}
