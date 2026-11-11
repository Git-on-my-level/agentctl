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
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestChildrenManagedAttributionAndCallerIsolation(t *testing.T) {
	root := testutil.TempDir(t)
	path := filepath.Join(root, "state", "journal.db")
	j, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parent := identityFixture(t, j, "codex", "session-parent")
	j.Close()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	env := map[string]string{"AGENTCTL_EXECUTION_ID": parent.ID.String(), "AGENTCTL_ADAPTER": "codex", "CODEX_THREAD_ID": "session-parent"}
	a.getenv = func(k string) string { return env[k] }
	args := []string{"--journal", path, "run", "--adapter", "generic-process", "--idempotency-key", "child-1", "--", "/bin/sh", "-c", `printf '%s\n' '{"type":"result","status":"completed","result":"child answer"}'`}
	if code := a.run(context.Background(), args); code != 0 {
		t.Fatalf("launch=%d %s", code, stdout.String())
	}
	var admitted struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	child := admitted.Result
	expected := sessionIdentity("codex", "session-parent", "native_stream", "observed")
	if child.ParentExecutionID == nil || *child.ParentExecutionID != parent.ID || child.CallerIdentity == nil || child.CallerIdentity.NativeSessionID != *expected.ID {
		t.Fatalf("child=%+v", child)
	}
	a.updateNotice = func(context.Context, string, common) *output.Warning {
		t.Fatal("read query triggered maintenance")
		return nil
	}
	query := func(extra ...string) (int, string) {
		stdout.Reset()
		code := a.run(context.Background(), append([]string{"--journal", path, "children"}, extra...))
		return code, stdout.String()
	}
	if code, out := query(); code != 0 || !strings.Contains(out, child.ID.String()) || !strings.Contains(out, "terminal_uncollected") {
		t.Fatalf("children=%d %s", code, out)
	}
	if code, out := query("--provider", "codex", "--native-session-id", "someone-else"); code != 0 || !strings.Contains(out, `"count":0`) {
		t.Fatalf("isolation=%d %s", code, out)
	}
	if code, out := query("--summary"); code != 0 || !strings.Contains(out, `"count":1`) || !strings.Contains(out, `"executions":[]`) {
		t.Fatalf("summary=%d %s", code, out)
	}
	// Read command must never acknowledge the result.
	if code, out := query(); code != 0 || !strings.Contains(out, child.ID.String()) {
		t.Fatalf("query collected child: %d %s", code, out)
	}
	env["CODEX_THREAD_ID"] = "conflicting-inherited-thread"
	if code, out := query(); code == 0 || !strings.Contains(out, "children_identity_unknown") {
		t.Fatalf("conflicting identity=%d %s", code, out)
	}
	// Explicit parent query ignores ambient conflicts.
	if code, out := query("--execution", parent.ID.String()); code != 0 || !strings.Contains(out, child.ID.String()) {
		t.Fatalf("explicit parent=%d %s", code, out)
	}
	// Keyed replay retains the original caller even if the current managed
	// environment points at an unavailable parent in this journal.
	a.updateNotice = nil
	env["AGENTCTL_EXECUTION_ID"] = remoteTestID
	stdout.Reset()
	if code := a.run(context.Background(), args); code != 0 {
		t.Fatalf("caller metadata blocked replay: %d %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), child.ID.String()) || !strings.Contains(stdout.String(), parent.ID.String()) {
		t.Fatalf("replay changed original ownership: %s", stdout.String())
	}
}

func TestChildrenExplicitSessionEnvironmentAndPrivacy(t *testing.T) {
	env := map[string]string{adapter.CallerProviderEnv: "claude-code", adapter.CallerSessionEnv: "private-claude-session", "CODEX_THREAD_ID": "inherited-parent"}
	code, report, raw := identityInvoke(t, env)
	if code != 0 || report.Provider.ID == nil || *report.Provider.ID != "claude" || report.NativeSession.ID == nil || strings.Contains(raw, env[adapter.CallerSessionEnv]) {
		t.Fatalf("identity=%d %s", code, raw)
	}
	a := &app{getenv: func(k string) string { return env[k] }}
	parent, caller, problem := a.launchAttribution(context.Background(), common{})
	if problem != nil || parent != nil || caller == nil || caller.NativeSessionID != *report.NativeSession.ID {
		t.Fatalf("attribution=%v %+v %v", parent, caller, problem)
	}
	filtered := adapter.WithoutCallerDeclaration([]string{adapter.CallerProviderEnv + "=claude", adapter.CallerSessionEnv + "=private-claude-session", "PATH=keep"})
	if len(filtered) != 1 || filtered[0] != "PATH=keep" {
		t.Fatalf("inherited session=%v", filtered)
	}
	env[adapter.CallerSessionEnv] = "private-invalid-session\n"
	_, _, problem = a.launchAttribution(context.Background(), common{})
	if problem == nil || problem.Code != output.CodeUsage || problem.Details["diagnostic_code"] != "caller_identity_invalid" {
		t.Fatalf("invalid declaration lost diagnostic: %v", problem)
	}
	encoded, _ := json.Marshal(problem)
	if bytes.Contains(encoded, []byte("private-invalid-session")) {
		t.Fatalf("invalid declaration leaked: %s", encoded)
	}
}

func TestChildrenReadLockFailsFast(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	j, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	start := time.Now()
	code := a.run(context.Background(), []string{"--journal", path, "children", "--provider", "claude", "--native-session-id", "session"})
	if code != 9 || !strings.Contains(stdout.String(), "children_query_incomplete") || time.Since(start) > 2*time.Second {
		t.Fatalf("locked=%d %s elapsed=%s", code, stdout.String(), time.Since(start))
	}
}

func TestChildrenMissingIdentityDoesNotCreateJournal(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "missing", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", path, "children"}); code == 0 || !strings.Contains(stdout.String(), "children_identity_unknown") {
		t.Fatalf("unknown=%d %s", code, stdout.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("query created journal", err)
	}
}

func TestChildrenRemoteForwardingRetainsLocalCaller(t *testing.T) {
	f := newRemoteFixture(t)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	env := map[string]string{adapter.CallerProviderEnv: "claude", adapter.CallerSessionEnv: "remote-launching-session"}
	a.getenv = func(k string) string { return env[k] }
	a.stdin = strings.NewReader("task")
	args := []string{"--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin"}
	if code := a.run(context.Background(), args); code != 0 {
		t.Fatalf("remote launch=%d %s", code, stdout.String())
	}
	var receipt struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(f.root, "ssh-argv"))
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", f.journal, "children"}); code != 0 || !strings.Contains(stdout.String(), receipt.Result.ID) || !strings.Contains(stdout.String(), remoteTestID) {
		t.Fatalf("remote children=%d %s", code, stdout.String())
	}
	after, _ := os.ReadFile(filepath.Join(f.root, "ssh-argv"))
	if !bytes.Equal(before, after) {
		t.Fatal("children contacted remote host")
	}
}

func TestManagedChildrenAndLaunchAttributionFailClosedOnContention(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	j, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	parent := identityFixture(t, j, "codex", "managed-parent")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	env := map[string]string{"AGENTCTL_EXECUTION_ID": parent.ID.String(), "AGENTCTL_ADAPTER": "codex", "CODEX_THREAD_ID": "managed-parent"}
	a.getenv = func(k string) string { return env[k] }
	if code := a.run(context.Background(), []string{"--journal", path, "children"}); code != 9 || !strings.Contains(stdout.String(), "children_query_incomplete") {
		t.Fatalf("contention became unknown/empty: %d %s", code, stdout.String())
	}
	// Remote launch attribution cannot proceed to transport without evidence.
	gotParent, caller, problem := a.launchAttribution(context.Background(), common{journalPath: path})
	if problem == nil || !problem.Retryable || gotParent != nil || caller != nil {
		t.Fatalf("contended attribution admitted: %v %+v %v", gotParent, caller, problem)
	}
	// Standalone identity remains a best-effort observation, preserving its API.
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", path, "identity"}); code != 0 {
		t.Fatalf("standalone identity changed: %d %s", code, stdout.String())
	}
}

func TestNativeAdmissionPreservesParentAfterContendedJournal(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	j, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parent := identityFixture(t, j, "codex", "managed-parent")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	env := map[string]string{"AGENTCTL_EXECUTION_ID": parent.ID.String(), "AGENTCTL_ADAPTER": "codex", "CODEX_THREAD_ID": "managed-parent"}
	a.getenv = func(k string) string { return env[k] }
	closed := make(chan struct{})
	time.AfterFunc(150*time.Millisecond, func() { _ = j.Close(); close(closed) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	code := a.run(ctx, []string{"--journal", path, "run", "--adapter", "generic-process", "--", "/bin/sh", "-c", `printf '%s\n' '{"type":"result","status":"completed","result":"answer"}'`})
	<-closed
	if code != 0 {
		t.Fatalf("contended launch: %d %s", code, stdout.String())
	}
	var doc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Result.ParentExecutionID == nil || *doc.Result.ParentExecutionID != parent.ID {
		t.Fatalf("lost parent after lock recovery: %+v", doc.Result)
	}
}

func TestChildrenAwaitRegistrationLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-held waiter locks unavailable")
	}
	for _, exit := range []string{"cancel", "timeout", "attention"} {
		t.Run(exit, func(t *testing.T) {
			path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
			j, err := store.Open(path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			fixture := identityFixture(t, j, "codex", "")
			fixture.ID = ""
			fixture.Revision = 0
			fixture.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: *sessionIdentity("codex", "waiter-test", "explicit", "self_reported").ID, Provenance: "explicit"}
			child, _, err := j.CreateExecution(context.Background(), fixture, contracts.MutationKey{})
			j.Close()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			args := []string{"--journal", path, "await", child.ID.String()}
			if exit == "timeout" {
				args = append(args, "--timeout", "1s")
			}
			done := make(chan int, 1)
			finished := make(chan struct{})
			go func() { defer close(finished); done <- a.run(ctx, args) }()
			defer func() { cancel(); <-finished }()
			query := func() int {
				t.Helper()
				var out, errs bytes.Buffer
				q := testApp(&out, &errs)
				if code := q.run(context.Background(), []string{"--journal", path, "children", "--provider", "codex", "--native-session-id", "waiter-test", "--summary"}); code != 0 {
					t.Fatalf("query=%d %s", code, out.String())
				}
				var doc struct {
					Result struct {
						Count   int `json:"count"`
						Running int `json:"running_without_known_waiter"`
					} `json:"result"`
				}
				if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
					t.Fatal(err)
				}
				if doc.Result.Count != doc.Result.Running {
					t.Fatalf("summary running mismatch: %s", out.String())
				}
				return doc.Result.Count
			}
			registered := false
			for until := time.Now().Add(800 * time.Millisecond); time.Now().Before(until); {
				if query() == 0 {
					registered = true
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !registered {
				t.Fatal("active await was counted as running without a waiter")
			}
			switch exit {
			case "cancel":
				cancel()
			case "attention":
				write, err := store.Open(path, store.Options{})
				if err != nil {
					t.Fatal(err)
				}
				current, err := write.GetExecution(context.Background(), child.ID)
				if err != nil {
					write.Close()
					t.Fatal(err)
				}
				current.State = model.StateAttention
				_, err = write.UpdateExecution(context.Background(), current, current.Revision)
				write.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			code := <-done
			want := map[string]int{"cancel": 12, "timeout": 9, "attention": 10}[exit]
			if code != want {
				t.Fatalf("await exit=%d want=%d %s", code, want, stdout.String())
			}
			if query() != 1 {
				t.Fatal("exited waiter hid the child")
			}
		})
	}
}

func TestAwaitContinuesWhenWaiterRegistrationFails(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-held waiter locks unavailable")
	}
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
			j, err := store.Open(path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			e := identityFixture(t, j, "codex", "")
			j.Close()
			// Deterministically prevent sidecar directory creation while keeping
			// the journal readable, regardless of the test user's privileges.
			if err := os.WriteFile(path+".waiters", nil, 0o600); err != nil {
				t.Fatal(err)
			}
			budget := 3 * time.Second
			if mode == "cancel" {
				budget = 120 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			args := []string{"--journal", path, "await", e.ID.String()}
			if mode == "timeout" {
				args = append(args, "--timeout", "120ms")
			}
			start := time.Now()
			code := a.run(ctx, args)
			want := 9
			if mode == "cancel" {
				want = 12
			}
			if code != want || time.Since(start) < 100*time.Millisecond {
				t.Fatalf("sidecar failure broke primary wait: exit=%d want=%d elapsed=%s %s", code, want, time.Since(start), stdout.String())
			}
		})
	}
}
