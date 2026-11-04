package anxrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCardSlugWordEntropy(t *testing.T) {
	for _, tc := range []struct {
		slug string
		ok   bool
	}{
		{"stop-the-recover-quarantine-poison-loop", true},
		{"alarm-on-batch-audio-unprocessed-2-hours", true},
		{"build-the-cheapest-long-term-streaming", true},
		{"ghp-ordinary-card", true},
		{"task-fix-the-worker", true},
		{"ask-about-the-task", true},
		{"mask-the-desk-risk", true},
		{"slovakia-trip", true},
		{"akia-fixture", false},
		{"task-sk-fixture", false},
		{"task-glpat-fixture", false},
		{"task-glrt-fixture", false},
		{"task-xoxb-fixture", false},
		{"task-pypi-fixture", false},
		{"sk-fixture", false},
		{"sk-abcdefghijklmnopqrstuvwxyz012345", false},
		{"glpat-abcdefghijklmnopqrstuvwxyz", false},
		{"abcdefghijklmnopqrstuvwxyz0123456789", false},
		{"card-abcdefghijklmnopqrstuvwxyz0123456789-end", false},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			if got := ParseCardSlug(tc.slug) == nil; got != tc.ok {
				t.Fatalf("accepted=%v want %v", got, tc.ok)
			}
			r := testRegistration("slug", PhaseStart, 1)
			r.CardRefs = []string{"card:" + tc.slug}
			policy := Verification{Cards: map[string]ConfirmedCard{r.CardRefs[0]: {Ref: r.CardRefs[0], ThreadID: "thread-test"}}}
			safe, err := Allowlisted(r, nil, policy)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(safe.CardRefs) == 1; got != tc.ok {
				t.Fatalf("egress accepted=%v code=%d", got, safe.EgressErrorCode)
			}
		})
	}
}

func TestWorkspaceCatalogSelection(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"ok":true,"result":{"base_url":"https://omi.test/","workspaces":[{"alias":"omi","base_url":"https://omi.test"},{"alias":"anx","base_url":"https://anx.test"}]}}`, "omi"},
		{`{"ok":true,"result":{"base_url":"https://omi.test","workspaces":[{"alias":"z","base_url":"https://omi.test"},{"alias":"a","base_url":"https://omi.test"}]}}`, "a"},
		{`{"ok":true,"result":{"base_url":"https://omi.test","selection_error":"ambiguous","workspaces":[]}}`, ""},
		{`{"ok":true,"result":{"base_url":"https://missing.test","workspaces":[]}}`, ""},
		{`{"ok":false}`, ""},
	} {
		got, err := workspaceFromCatalog([]byte(tc.raw), false)
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Fatalf("got %q %v want %q", got, err, tc.want)
		}
	}
}

func TestWorkspaceResolverUsesRunCWDAndNoInheritedWorkspace(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "omi")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "fake-anx")
	body := `#!/bin/sh
[ "$PWD" = "$EXPECTED_ANX_CWD" ] || exit 81
[ -z "$ANX_WORKSPACE$ANX_BASE_URL" ] || exit 82
[ "$ANX_UPDATE_POLICY" = off ] || exit 83
[ "$*" = '--json --config-dir /fixture --timeout 1s config workspaces' ] || exit 84
printf '%s' '{"ok":true,"result":{"base_url":"https://omi.test","workspaces":[{"alias":"omi","base_url":"https://omi.test"}]}}'
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXPECTED_ANX_CWD", physical)
	t.Setenv("ANX_WORKSPACE", "anx")
	t.Setenv("ANX_BASE_URL", "https://wrong.test")
	sender := NewCLISender(CLIOptions{Executable: script, ConfigDir: "/fixture", Timeout: time.Second})
	r := testRegistration("cwd", PhaseStart, 1)
	r.CWD = cwd
	got, err := sender.Prepare(context.Background(), r)
	if err != nil || got.WorkspaceAlias != "omi" {
		t.Fatalf("got %+v %v", got, err)
	}
	// An explicit host override does not need an existing cwd or executable.
	sender = NewCLISender(CLIOptions{Executable: "/missing", WorkspaceAlias: "anx"})
	r.CWD = ""
	got, err = sender.Prepare(context.Background(), r)
	if err != nil || got.WorkspaceAlias != "anx" {
		t.Fatalf("override %+v %v", got, err)
	}
	got.WorkspaceAlias = "omi"
	got, err = sender.Prepare(context.Background(), got)
	if err != nil || got.WorkspaceAlias != "omi" {
		t.Fatalf("pinned alias overwritten %+v %v", got, err)
	}
}

func TestMixedWorkspaceAndAdapterDelivery(t *testing.T) {
	runner := &fakeRunner{}
	sender := &CLISender{run: runner.run}
	for _, tc := range []struct{ workspace, adapter, identity string }{{"omi", "claude-code", "claude"}, {"anx", "claude-code", "claude"}, {"anx", "codex", "codex"}, {"omi", "multica", "multica"}} {
		before := len(runner.argvs)
		r := testRegistration(tc.workspace+tc.adapter, PhaseTerminal, 1)
		r.Adapter = tc.adapter
		r.WorkspaceAlias = tc.workspace
		if tc.adapter == "multica" {
			r.NativeSession = nil
		}
		if err := sender.Send(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(runner.argvs[before], " "), "api cards.list") {
			t.Fatal("catalog not scoped to this run")
		}
		for i, argv := range runner.argvs[before:] {
			joined := strings.Join(argv, " ")
			if !strings.Contains(joined, "--workspace "+tc.workspace) || !strings.Contains(joined, "--as "+tc.identity) {
				t.Fatalf("wrong route/identity %v", argv)
			}
			if strings.Contains(joined, "sessions.register") && strings.Contains(runner.stdins[before+i], "host_scope") {
				t.Fatal("agentctl host identity sent to ANX")
			}
		}
	}
}

func TestWorkspacePinPersistedBeforeRemoteAndRetry(t *testing.T) {
	spool := testSpool(t)
	r := testRegistration("pin", PhaseStart, 0)
	r.CWD = "/fixture"
	if _, err := spool.Enqueue(r); err != nil {
		t.Fatal(err)
	}
	selectionCalls := 0
	runner := &fakeRunner{}
	sender := &CLISender{run: func(ctx context.Context, argv []string, body []byte) ([]byte, error) {
		if strings.Contains(strings.Join(argv, " "), "config workspaces") {
			selectionCalls++
			return []byte(`{"ok":true,"result":{"base_url":"https://omi.test","workspaces":[{"alias":"omi","base_url":"https://omi.test"}]}}`), nil
		}
		entries, err := spool.ListPending()
		if err != nil || len(entries) != 1 || entries[0].Registration.WorkspaceAlias != "omi" {
			t.Fatalf("remote call before durable pin: %+v %v", entries, err)
		}
		if selectionCalls == 1 {
			return []byte(`{"ok":false,"error":{"code":"sessions_unavailable"}}`), nil
		}
		return runner.run(ctx, argv, body)
	}}
	result, err := spool.Flush(context.Background(), sender, 0)
	if err != nil || result.Retried != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	entries, _ := spool.ListPending()
	entries[0].NextAttemptAt = time.Time{}
	if err := rewriteEntry(spool, entries[0]); err != nil {
		t.Fatal(err)
	}
	sender.options.WorkspaceAlias = "anx"
	result, err = spool.Flush(context.Background(), sender, 0)
	if err != nil || result.Retried != 1 || selectionCalls != 1 {
		t.Fatalf("changed pin: %+v calls=%d %v", result, selectionCalls, err)
	}
}

func TestRequeueDeadDryRunCapacityAndReplay(t *testing.T) {
	spool := testSpool(t)
	for _, id := range []string{"one", "two", "three"} {
		if _, err := spool.Enqueue(testRegistration(id, PhaseTerminal, 0)); err != nil {
			t.Fatal(err)
		}
	}
	fail := &recordingSender{failures: 100, fatalOnce: true}
	if _, err := spool.Flush(context.Background(), fail, 0); err != nil {
		t.Fatal(err)
	}
	dead, err := spool.ListDead()
	if err != nil || len(dead) != 3 {
		t.Fatalf("%+v %v", dead, err)
	}
	snapshot := map[string]string{}
	if err := filepath.Walk(spool.Root(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			snapshot[path] = string(raw)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	spool.maxPending = 2
	result, err := spool.RequeueDead(context.Background(), true)
	if err != nil || result.Requeued != 2 || result.DeferredCapacity != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	for path, want := range snapshot {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatalf("dry run mutated %s", path)
		}
	}
	result, err = spool.RequeueDead(context.Background(), false)
	if err != nil || result.Requeued != 2 || result.DeferredCapacity != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	pending, _ := spool.ListPending()
	for _, entry := range pending {
		found := false
		for _, old := range dead {
			if old.ExecutionID == entry.ExecutionID {
				found = true
				if !reflect.DeepEqual(old.Registration, entry.Registration) || old.CreatedAt != entry.CreatedAt || entry.Attempts != 0 || entry.LastError != "" {
					t.Fatalf("payload/history changed %+v", entry)
				}
			}
		}
		if !found {
			t.Fatal("unknown entry")
		}
	}
	result, err = spool.RequeueDead(context.Background(), false)
	if err != nil || result.Requeued != 0 || result.DeferredCapacity != 1 {
		t.Fatalf("repeat %+v %v", result, err)
	}
	// Crash after writing pending, before deleting dead: pending wins.
	raw, _ := json.Marshal(pending[0])
	if err := os.WriteFile(spool.deadPath(entryKey(pending[0].ExecutionID, pending[0].Phase)), raw, 0600); err != nil {
		t.Fatal(err)
	}
	result, err = spool.RequeueDead(context.Background(), false)
	if err != nil || result.AlreadyPending != 1 {
		t.Fatalf("crash replay %+v %v", result, err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := spool.RequeueDead(context.Background(), false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	success := &recordingSender{}
	if _, err := spool.Flush(context.Background(), success, 0); err != nil {
		t.Fatal(err)
	}
	// A committed registration's leftover dead copy must never be sent again.
	if err := os.WriteFile(spool.deadPath(entryKey(pending[0].ExecutionID, pending[0].Phase)), raw, 0600); err != nil {
		t.Fatal(err)
	}
	result, err = spool.RequeueDead(context.Background(), false)
	if err != nil || result.AlreadyDelivered != 1 || result.Requeued != 1 {
		t.Fatalf("delivered replay %+v %v", result, err)
	}
}

func TestWorkspaceRoutingDoesNotChangeRequestKey(t *testing.T) {
	runner := &fakeRunner{}
	sender := &CLISender{run: runner.run}
	r := testRegistration("request-key", PhaseTerminal, 1)
	for _, alias := range []string{"anx", "omi"} {
		r.WorkspaceAlias = alias
		if err := sender.Send(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	keys := []string{}
	for i, argv := range runner.argvs {
		if strings.Contains(strings.Join(argv, " "), "events.create") {
			var body struct {
				RequestKey string `json:"request_key"`
			}
			if err := json.Unmarshal([]byte(runner.stdins[i]), &body); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(runner.stdins[i], "workspace_alias") {
				t.Fatal("local routing entered public payload")
			}
			keys = append(keys, body.RequestKey)
		}
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("changed keys %v", keys)
	}
}

func TestWorkspacePinSharedByPhasesAndReenqueue(t *testing.T) {
	spool := testSpool(t)
	r := testRegistration("shared-pin", PhaseStart, 0)
	r.WorkspaceAlias = "omi"
	first, err := spool.Enqueue(r)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkspaceAlias = "anx"
	again, err := spool.Enqueue(r)
	if err != nil || again.Registration.WorkspaceAlias != "omi" || again.Registration.Sequence != first.Registration.Sequence {
		t.Fatalf("refresh changed routing %+v %v", again, err)
	}
	r.Phase = PhaseTerminal
	terminal, err := spool.Enqueue(r)
	if err != nil || terminal.Registration.WorkspaceAlias != "omi" {
		t.Fatalf("phase changed routing %+v %v", terminal, err)
	}
}

func TestWorkspacePreparationFailureRetriesWithoutSending(t *testing.T) {
	spool := testSpool(t)
	r := testRegistration("unresolved-workspace", PhaseTerminal, 0)
	r.CWD = "/fixture"
	if _, err := spool.Enqueue(r); err != nil {
		t.Fatal(err)
	}
	calls := 0
	sender := &CLISender{run: func(_ context.Context, argv []string, _ []byte) ([]byte, error) {
		calls++
		if !strings.Contains(strings.Join(argv, " "), "config workspaces") {
			t.Fatal("remote call after unresolved workspace")
		}
		return []byte(`{"ok":true,"result":{"selection_error":"ambiguous"}}`), nil
	}}
	result, err := spool.Flush(context.Background(), sender, 0)
	if err != nil || result.Retried != 1 || result.DeadLettered != 0 || calls != 1 {
		t.Fatalf("%+v calls=%d %v", result, calls, err)
	}
}

func TestWorkspacePreparationDoesNotOverwriteRefresh(t *testing.T) {
	spool := testSpool(t)
	r := testRegistration("refresh-workspace", PhaseStart, 0)
	r.CWD = "/fixture"
	if _, err := spool.Enqueue(r); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	sender := &CLISender{run: func(_ context.Context, argv []string, _ []byte) ([]byte, error) {
		if !strings.Contains(strings.Join(argv, " "), "config workspaces") {
			t.Error("stale registration sent")
			return nil, context.Canceled
		}
		close(entered)
		<-release
		return []byte(`{"ok":true,"result":{"base_url":"https://omi.test","workspaces":[{"alias":"omi","base_url":"https://omi.test"}]}}`), nil
	}}
	done := make(chan error, 1)
	go func() { _, err := spool.Flush(context.Background(), sender, 0); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not start")
	}
	r.State = StateCompleted
	_, enqueueErr := spool.Enqueue(r)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if enqueueErr != nil {
		t.Fatal(enqueueErr)
	}
	entries, err := spool.ListPending()
	if err != nil || len(entries) != 1 || entries[0].Registration.State != StateCompleted || entries[0].Registration.WorkspaceAlias != "" {
		t.Fatalf("refresh overwritten %+v %v", entries, err)
	}
}
