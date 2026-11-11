package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

func invokeAttributedRemote(t *testing.T, f remoteFixture, session, prompt string) (int, string) {
	t.Helper()
	var out, errs bytes.Buffer
	a := testApp(&out, &errs)
	a.getenv = func(k string) string {
		switch k {
		case adapter.CallerProviderEnv:
			return "claude"
		case adapter.CallerSessionEnv:
			return session
		}
		return ""
	}
	a.stdin = strings.NewReader(prompt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := a.run(ctx, []string{"--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin"})
	return code, out.String()
}

func queryRemoteChildren(t *testing.T, f remoteFixture, session string, want int) []store.ChildExecution {
	t.Helper()
	before, sshBefore := mustRead(t, f.journal), mustRead(t, filepath.Join(f.root, "ssh-argv"))
	code, out := f.command("", "children", "--provider", "claude", "--native-session-id", session)
	var doc struct {
		Result struct {
			Complete   bool                   `json:"complete"`
			Coverage   string                 `json:"coverage"`
			Count      int                    `json:"count"`
			Executions []store.ChildExecution `json:"executions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if code != 0 || !doc.Result.Complete || doc.Result.Coverage != "retained_attributed_executions_only" || doc.Result.Count != want || len(doc.Result.Executions) != want {
		t.Fatalf("retained child coverage: exit=%d want=%d %s", code, want, out)
	}
	if strings.Contains(out, session) {
		t.Fatal("raw caller session leaked")
	}
	if !bytes.Equal(before, mustRead(t, f.journal)) || !bytes.Equal(sshBefore, mustRead(t, filepath.Join(f.root, "ssh-argv"))) {
		t.Fatal("children mutated the journal or contacted SSH")
	}
	return doc.Result.Executions
}

func TestChildrenRemoteExactAwaitCoverageAndRelease(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-held waiter locks unavailable")
	}
	f := newRemoteFixture(t)
	const session = "private-remote-waiter-session"
	code, out := invokeAttributedRemote(t, f, session, "private remote task")
	if code != 0 {
		t.Fatal(out)
	}
	local := remoteLocalID(t, out)
	e := f.execution(t, local)
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = j.PutSubscription(context.Background(), subscription.Subscription{
		Origin: e.OriginHostID.String(), State: subscription.StateActive, ExpiresAt: time.Now().Add(time.Hour),
		Filter:      subscription.EventFilter{ExecutionIDs: []string{local}, Kinds: []string{"terminal"}},
		Destination: callback.Destination{Kind: callback.DestinationStdout},
	})
	j.Close()
	if err != nil {
		t.Fatal(err)
	}
	queryRemoteChildren(t, f, session, 1) // a local subscription is not remote evidence
	path := filepath.Join(f.root, "fake-ssh")
	script := strings.Replace(string(mustRead(t, path)), `*"'await'"*) cat terminal ;;`, `*"'await'"*) touch awaiting; exec sleep 10 ;;`, 1)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	done := make(chan struct{})
	var waitCode int
	go func() {
		defer close(done)
		waitCode = a.run(ctx, []string{"--journal", f.journal, "await", local, "--content"})
	}()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(2 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(f.root, "awaiting")); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatalf("await exited: %d %s", waitCode, stdout.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("remote await did not reach SSH")
		}
		time.Sleep(time.Millisecond)
	}
	queryRemoteChildren(t, f, session, 0)
	// Registration and remote collection must not retain a journal lock.
	write, err := store.Open(f.journal, store.Options{LockTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("remote await holds journal: %v", err)
	}
	write.Close()
	cancel()
	<-done
	if waitCode != 12 {
		t.Fatalf("cancellation: %d %s", waitCode, stdout.String())
	}
	queryRemoteChildren(t, f, session, 1)
	if f.execution(t, local).State != model.StateRunning {
		t.Fatal("cancelled transport changed execution state")
	}
	live, err := store.RegisterWaiter(context.Background(), f.journal, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	for _, state := range []model.State{model.StateAttention, model.StateCompleted} {
		write, err := store.Open(f.journal, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		next, err := write.GetExecution(context.Background(), e.ID)
		if err != nil {
			write.Close()
			t.Fatal(err)
		}
		next.State = state
		if state.Terminal() {
			now := time.Now().UTC()
			next.TerminalAt, next.Liveness = &now, model.LivenessExited
		}
		_, err = write.UpdateExecution(context.Background(), next, next.Revision)
		write.Close()
		if err != nil {
			t.Fatal(err)
		}
		queryRemoteChildren(t, f, session, 1) // waiters never suppress attention or uncollected terminals
	}
}

func TestChildrenRemoteLostReceiptRetainedCoverageAndRecovery(t *testing.T) {
	f := newRemoteFixture(t)
	const session, prompt = "private-lost-receipt-caller", "private lost receipt task"
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	path := filepath.Join(f.root, "fake-ssh")
	script := strings.Replace(string(mustRead(t, path)), "cat receipt", `if [ ! -f dropped ]; then touch dropped; echo lost-receipt >&2; exit 255; fi; cat receipt`, 1)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	code, out := invokeAttributedRemote(t, f, session, prompt)
	if code != 11 || !strings.Contains(out, `"retryable":true`) || !strings.Contains(out, "retry the same request key") {
		t.Fatalf("uncertain admission was not preserved: %d %s", code, out)
	}
	if string(mustRead(t, filepath.Join(f.root, "launched"))) != "launch\n" {
		t.Fatal("fixture did not launch before losing receipt")
	}
	// Complete refers only to retained attributed rows. This empty index does
	// not prove remote non-launch; the uncertain error still requires recovery.
	queryRemoteChildren(t, f, session, 0)
	code, out = invokeAttributedRemote(t, f, session, prompt)
	if code != 0 {
		t.Fatalf("identical-key recovery: %d %s", code, out)
	}
	local := remoteLocalID(t, out)
	items := queryRemoteChildren(t, f, session, 1)
	if items[0].ID.String() != local || items[0].Remote == nil || items[0].Remote.ExecutionID.String() != remoteTestID {
		t.Fatalf("recovered child lost local ownership/provenance: %+v", items)
	}
	if string(mustRead(t, filepath.Join(f.root, "launched"))) != "launch\n" {
		t.Fatal("recovery duplicated remote launch")
	}
	journal := string(mustRead(t, f.journal))
	if strings.Contains(journal, session) || strings.Contains(journal, prompt) {
		t.Fatal("journal retained raw caller session or prompt")
	}
}
