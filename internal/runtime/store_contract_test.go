package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/runtime"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// Keep the CLI wiring boundary honest as store evolves independently.
var _ runtime.Journal = (*store.Journal)(nil)

func TestEngineWritesStoreAcceptedSemanticEvents(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "fixture-agent")
	body := "#!/bin/sh\nset -eu\nprintf '%s\\n' '{\"type\":\"session.started\",\"session_id\":\"store-session\"}' '{\"type\":\"result\",\"result\":\"bounded summary\",\"is_error\":false}'\n"
	if err := os.WriteFile(executable, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	journal, err := store.Open(filepath.Join(root, "state", "journal.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	engine, err := runtime.New(journal, runtime.Options{})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := engine.Launch(context.Background(), runtime.LaunchOptions{
		Adapter: runtime.AdapterSpec{Name: "generic", Executable: executable},
		Request: adapter.LaunchRequest{Argv: []string{executable}, DiscoveryWindow: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != model.StateCompleted {
		t.Fatalf("execution state = %s", execution.State)
	}
	events, err := journal.ListEvents(context.Background(), execution.ID, contracts.EventQuery{})
	if err != nil || len(events) != 1 || events[0].Kind != model.EventTerminal {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}
