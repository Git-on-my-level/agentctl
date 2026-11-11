package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func addExitChild(t *testing.T, j *store.Journal, parent model.Execution, terminal bool) model.Execution {
	t.Helper()
	child := parent
	child.ID = ids.ExecutionID("")
	child.Revision = 0
	child.Delegation = nil
	child.SourceBindings = nil
	child.ParentExecutionID = &parent.ID
	child.Supersedes = nil
	child.SupersededBy = nil
	child.AutoContinuationOf = nil
	child.UncollectedChildren = nil
	child.ChildExitGuard = nil
	child.State = model.StateRunning
	child.TerminalAt = nil
	if terminal {
		child.State = model.StateCompleted
		now := time.Now().UTC()
		child.TerminalAt = &now
	}
	child, _, err := j.CreateExecution(context.Background(), child, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func TestChildExitFlagAndContinuationExactlyOnce(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	code, out := f.invoke("parent task")
	if code != 0 {
		t.Fatalf("parent: %d %s", code, out)
	}
	id, _ := ids.ParseExecutionID(resultID(t, out))
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := j.GetExecution(context.Background(), id)
	child := addExitChild(t, j, parent, true)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	c := common{configPath: f.config, journalPath: f.journal}
	parent = a.checkChildExit(context.Background(), c, j, parent, time.Now().UTC())
	if parent.ChildExitGuard == nil || parent.ChildExitGuard.AutoContinue != "pending" || len(parent.UncollectedChildren) != 1 {
		t.Fatalf("guard: %+v", parent)
	}
	parent, err = j.UpdateExecution(context.Background(), parent, parent.Revision)
	j.Close()
	if err != nil {
		t.Fatal(err)
	}
	prompt := childExitPrompt(parent)
	if !strings.Contains(prompt, child.ID.String()) || !strings.Contains(prompt, remoteCommand(parent.UncollectedChildren[0].CollectArgv...)) {
		t.Fatal(prompt)
	}
	for range 2 {
		stdout.Reset()
		if problem := a.finishNativeTurn(context.Background(), output.Renderer{Mode: output.JSON, Writer: &stdout}, c, parent); problem != nil {
			t.Fatal(problem)
		}
	}
	rawJournal, err := os.ReadFile(f.journal)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawJournal, []byte("This is the only automatic continuation")) {
		t.Fatal("automatic prompt was journaled")
	}
	if f.launches(t) != 2 {
		t.Fatalf("launches=%d, want original and one cleanup turn", f.launches(t))
	}
	j, _ = store.Open(f.journal, store.Options{})
	defer j.Close()
	parent, _ = j.GetExecution(context.Background(), id)
	if parent.ChildExitGuard.AutoContinue != "launched" || parent.ChildExitGuard.ContinuationID == nil {
		t.Fatalf("guard=%+v stderr=%s", parent.ChildExitGuard, stderr.String())
	}
	successor, err := j.GetExecution(context.Background(), *parent.ChildExitGuard.ContinuationID)
	if err != nil || successor.AutoContinuationOf == nil || *successor.AutoContinuationOf != id {
		t.Fatalf("successor=%+v err=%v", successor, err)
	}
	addExitChild(t, j, successor, false)
	successor = a.checkChildExit(context.Background(), c, j, successor, time.Now().UTC())
	if successor.ChildExitGuard == nil || successor.ChildExitGuard.AutoContinue != "suppressed_recursive" {
		t.Fatalf("recursion=%+v", successor.ChildExitGuard)
	}
	// A changed owner observation must preserve the original immutable marker.
	candidate := successor
	candidate.AutoContinuationOf = nil
	if _, err := j.UpdateExecution(context.Background(), candidate, candidate.Revision); err == nil {
		t.Fatal("automatic provenance was mutable")
	}
}

func TestChildExitPolicyStates(t *testing.T) {
	for _, variant := range []string{"no_children", "disabled", "opencode", "cancelled", "failed", "query_failure"} {
		t.Run(variant, func(t *testing.T) {
			f := newDelegateFixture(t, continueAgent)
			code, out := f.invoke("parent task")
			if code != 0 {
				t.Fatalf("%d %s", code, out)
			}
			id, _ := ids.ParseExecutionID(resultID(t, out))
			j, err := store.Open(f.journal, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			parent, _ := j.GetExecution(context.Background(), id)
			if variant != "no_children" {
				addExitChild(t, j, parent, false)
			}
			if variant == "disabled" {
				cfg, err := config.Load(f.config)
				if err != nil {
					t.Fatal(err)
				}
				profile := cfg.Profiles["test"]
				disabled := false
				profile.Delegation = &config.DelegationPolicy{ChildExitAutoContinue: &disabled}
				cfg.Profiles["test"] = profile
				raw, _ := json.Marshal(cfg)
				if err := os.WriteFile(f.config, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch variant {
			case "opencode":
				parent.Adapter = "opencode"
			case "cancelled":
				parent.State = model.StateCancelled
			case "failed":
				parent.State = model.StateFailed
			}
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			c := common{configPath: f.config, journalPath: f.journal}
			ctx := context.Background()
			if variant == "query_failure" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			checked := a.checkChildExit(ctx, c, j, parent, time.Now().UTC())
			if variant == "no_children" {
				if checked.ChildExitGuard != nil {
					t.Fatal("childless run changed")
				}
				return
			}
			if checked.ChildExitGuard == nil {
				t.Fatal("missing exit guard")
			}
			want := "pending"
			switch variant {
			case "disabled":
				want = "disabled"
			case "opencode", "query_failure", "failed":
				want = "unavailable"
			case "cancelled":
				want = "parent_not_completed"
			}
			if checked.ChildExitGuard.AutoContinue != want {
				t.Fatalf("got %+v want %s", checked.ChildExitGuard, want)
			}
			if variant == "query_failure" {
				if checked.ChildExitGuard.CheckError != "children_query_incomplete" {
					t.Fatal(checked.ChildExitGuard)
				}
				return
			}
			if len(checked.UncollectedChildren) != 1 {
				t.Fatal(checked.UncollectedChildren)
			}
			// Inbox flags even an already acknowledged parent's stranded children.
			item, actionable := projectInbox(checked, time.Now().UTC(), time.Hour, store.AcknowledgementIndex{}, output.JSON)
			if !actionable || len(item.UncollectedChildren) != 1 || !strings.Contains(fmt.Sprint(item.Reasons), "uncollected_children") {
				t.Fatalf("inbox=%+v", item)
			}
		})
	}
}

func TestChildExitOutcomeCommitCarriesSnapshot(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	code, out := f.invoke("parent task")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	id, _ := ids.ParseExecutionID(resultID(t, out))
	template, _ := j.GetExecution(context.Background(), id)
	parent := addExitChild(t, j, template, false)
	child := addExitChild(t, j, parent, true)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	next, err := a.finalizeResult(context.Background(), common{configPath: f.config, journalPath: f.journal}, j, parent, adapter.Result{Success: true, State: adapter.StateCompleted, Content: "parent answer"}, time.Now().UTC(), false)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := j.GetOutcome(context.Background(), parent.ID)
	if err != nil || len(outcome.UncollectedChildren) != 1 || outcome.UncollectedChildren[0].ID != child.ID || next.ChildExitGuard == nil {
		t.Fatalf("outcome=%+v next=%+v err=%v", outcome, next, err)
	}
}

// The public await registers before blocking, including remote forwarding, and
// releases on timeout/cancellation. Reading children never supplies a waiter.
func TestAwaitRegistersLiveWaiter(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("kernel waiter evidence")
	}
	f := newDelegateFixture(t, continueAgent)
	code, out := f.invoke("parent")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parentID, _ := ids.ParseExecutionID(resultID(t, out))
	parent, _ := j.GetExecution(context.Background(), parentID)
	child := addExitChild(t, j, parent, false)
	j.Close()
	done := make(chan int, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var out, stderr bytes.Buffer
		a := testApp(&out, &stderr)
		done <- a.await(ctx, output.Renderer{Mode: output.JSON, Writer: &out}, common{configPath: f.config, journalPath: f.journal}, []string{child.ID.String(), "--no-timeout"})
	}()
	reader := store.Reader{Open: func(context.Context, bool) (*store.Journal, error) {
		return store.Open(f.journal, store.Options{ReadOnly: true, LockTimeout: time.Millisecond})
	}}
	deadline := time.Now().Add(3 * time.Second)
	for {
		items, err := reader.Children(context.Background(), store.ChildrenScope{Parent: &parentID}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("await never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("await failed to stop")
	}
	items, err := reader.Children(context.Background(), store.ChildrenScope{Parent: &parentID}, time.Now())
	if err != nil || len(items) != 1 {
		t.Fatalf("waiter survived cancellation: %+v %v", items, err)
	}
	entries, _ := filepath.Glob(f.journal + ".waiters/" + child.ID.String() + "/*")
	if len(entries) != 0 {
		t.Fatal(entries)
	}
}

func TestChildExitFlagsRemoteForwardingWithoutSSH(t *testing.T) {
	f := newRemoteFixture(t)
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parent := identityFixture(t, j, "codex", "remote-parent-session")
	j.Close()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	env := map[string]string{"AGENTCTL_EXECUTION_ID": parent.ID.String(), "AGENTCTL_ADAPTER": "codex", "CODEX_THREAD_ID": "remote-parent-session"}
	a.getenv = func(k string) string { return env[k] }
	a.stdin = strings.NewReader("remote child task")
	args := []string{"--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin"}
	if code := a.run(context.Background(), args); code != 0 {
		t.Fatalf("remote child: %d %s", code, stdout.String())
	}
	childID, _ := ids.ParseExecutionID(resultID(t, stdout.String()))
	before, _ := os.ReadFile(filepath.Join(f.root, "ssh-argv"))
	j, err = store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parent, _ = j.GetExecution(context.Background(), parent.ID)
	c := common{journalPath: f.journal, configPath: f.config}
	parent, err = a.finalizeResult(context.Background(), c, j, parent, adapter.Result{Success: true, State: adapter.StateCompleted, Content: "parent answer"}, time.Now().UTC(), false)
	j.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.UncollectedChildren) != 1 || parent.UncollectedChildren[0].ID != childID || parent.UncollectedChildren[0].Remote == nil {
		t.Fatalf("remote snapshot=%+v", parent.UncollectedChildren)
	}
	if !strings.Contains(childExitPrompt(parent), childID.String()) || strings.Contains(childExitPrompt(parent), "'await' '"+remoteTestID+"'") {
		t.Fatal(childExitPrompt(parent))
	}
	after, _ := os.ReadFile(filepath.Join(f.root, "ssh-argv"))
	if !bytes.Equal(before, after) {
		t.Fatal("exit query contacted SSH")
	}
	// Awaiting the local forwarding ID is valid waiter evidence; unlike a
	// local subscription, the await actually observes the remote authority.
	waiter, err := store.RegisterWaiter(f.journal, childID)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	j, err = store.Open(f.journal, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	children, err := j.Children(context.Background(), store.ChildrenScope{Parent: &parent.ID}, time.Now().UTC())
	if err != nil || len(children) != 0 {
		t.Fatalf("remote waiter=%+v %v", children, err)
	}
}

func TestChildExitInboxClearsAfterCollection(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	code, out := f.invoke("parent")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	id, _ := ids.ParseExecutionID(resultID(t, out))
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := j.GetExecution(context.Background(), id)
	child := addExitChild(t, j, parent, true)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	c := common{journalPath: f.journal, configPath: f.config}
	parent = a.checkChildExit(context.Background(), c, j, parent, time.Now().UTC())
	parent, err = j.UpdateExecution(context.Background(), parent, parent.Revision)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = j.AcknowledgeExecution(context.Background(), parent.ID, store.AcknowledgementResult)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	if code, out := f.command(t, "", "inbox"); code != 0 || !strings.Contains(out, "uncollected_children") {
		t.Fatalf("inbox=%d %s", code, out)
	}
	j, err = store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = j.AcknowledgeExecution(context.Background(), child.ID, store.AcknowledgementResult)
	j.Close()
	if err != nil {
		t.Fatal(err)
	}
	if code, out := f.command(t, "", "inbox"); code != 0 || strings.Contains(out, "uncollected_children") {
		t.Fatalf("resolved inbox=%d %s", code, out)
	}
}

func TestAwaitAnyWaiterCoverage(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("kernel waiter evidence")
	}
	for _, selector := range []string{"explicit", "label", "attention_only", "timestamp_exclusion"} {
		t.Run(selector, func(t *testing.T) {
			f := newDelegateFixture(t, continueAgent)
			code, out := f.invoke("parent")
			if code != 0 {
				t.Fatalf("%d %s", code, out)
			}
			j, err := store.Open(f.journal, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			id, _ := ids.ParseExecutionID(resultID(t, out))
			parent, _ := j.GetExecution(context.Background(), id)
			parent.Labels = []string{"exitwait.child"}
			child := addExitChild(t, j, parent, false)
			j.Close()
			args := []string{"--execution", child.ID.String(), "--kind", "terminal", "--timeout", "2s"}
			if selector == "label" {
				args = []string{"--label-prefix", "exitwait.", "--kind", "terminal", "--timeout", "2s"}
			}
			if selector == "attention_only" {
				args = []string{"--execution", child.ID.String(), "--kind", "attention", "--timeout", "150ms"}
			}
			if selector == "timestamp_exclusion" {
				args = []string{"--execution", child.ID.String(), "--kind", "terminal", "--timeout", "150ms", "--since", time.Now().UTC().Format(time.RFC3339Nano)}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan int, 1)
			go func() {
				var out, stderr bytes.Buffer
				a := testApp(&out, &stderr)
				done <- a.awaitAny(ctx, output.Renderer{Mode: output.JSON, Writer: &out}, common{journalPath: f.journal, configPath: f.config}, args)
			}()
			expectCovered := selector == "explicit" || selector == "label"
			reader := store.Reader{Open: func(context.Context, bool) (*store.Journal, error) {
				return store.Open(f.journal, store.Options{ReadOnly: true, LockTimeout: time.Millisecond})
			}}
			if expectCovered {
				deadline := time.Now().Add(time.Second)
				for {
					children, err := reader.Children(context.Background(), store.ChildrenScope{Parent: &id}, time.Now())
					if err != nil {
						t.Fatal(err)
					}
					if len(children) == 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("no await-any waiter: %v", args)
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}
			select {
			case code := <-done:
				if code == 0 {
					t.Fatal("unexpected terminal match")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("await-any did not stop")
			}
			children, err := reader.Children(context.Background(), store.ChildrenScope{Parent: &id}, time.Now())
			if err != nil || len(children) != 1 {
				t.Fatalf("coverage survived exit: %+v %v", children, err)
			}
		})
	}
}

func TestChildExitExplicitCancellationSnapshot(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	code, out := f.invoke("template")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	id, _ := ids.ParseExecutionID(resultID(t, out))
	template, _ := j.GetExecution(context.Background(), id)
	parent := addExitChild(t, j, template, false)
	child := addExitChild(t, j, parent, false)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	c := common{configPath: f.config, journalPath: f.journal}
	now := time.Now().UTC()
	cancelled, err := commitCancellation(context.Background(), j, parent, now, func(next model.Execution) model.Execution {
		return a.checkChildExit(context.Background(), c, j, next, now)
	})
	if err != nil || cancelled.State != model.StateCancelled || cancelled.ChildExitGuard == nil || cancelled.ChildExitGuard.AutoContinue != "parent_not_completed" || len(cancelled.UncollectedChildren) != 1 || cancelled.UncollectedChildren[0].ID != child.ID {
		t.Fatalf("cancelled=%+v err=%v", cancelled, err)
	}
	outcome, err := j.GetOutcome(context.Background(), parent.ID)
	if err != nil || len(outcome.UncollectedChildren) != 1 {
		t.Fatalf("cancelled outcome=%+v err=%v", outcome, err)
	}
}
