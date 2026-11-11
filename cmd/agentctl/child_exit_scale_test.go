package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

func TestChildExitInboxCollectedHistoryCannotStarveActiveParent(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	ctx := context.Background()
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	c := common{configPath: f.config, journalPath: f.journal}
	historical := map[ids.ExecutionID]bool{}
	makePair := func(collected bool) (model.Execution, model.Execution) {
		t.Helper()
		parent := identityFixture(t, j, "codex", "inbox-scale")
		child := addExitChild(t, j, parent, collected)
		parent, err = a.finalizeResult(ctx, c, j, parent, adapter.Result{Success: true, State: adapter.StateCompleted, Content: "parent answer"}, time.Now().UTC(), false)
		if err != nil {
			t.Fatal(err)
		}
		if !parent.ChildExitRecheck || len(parent.UncollectedChildren) != 1 {
			t.Fatal("fixture did not retain exit evidence")
		}
		if _, _, err = j.AcknowledgeExecution(ctx, parent.ID, store.AcknowledgementAwait); err != nil {
			t.Fatal(err)
		}
		if collected {
			if _, _, err = j.AcknowledgeExecution(ctx, child.ID, store.AcknowledgementAwait); err != nil {
				t.Fatal(err)
			}
		}
		return parent, child
	}
	// The active parent is older than every historical marker, so a reverse
	// listing encounters the history before reaching this real obligation.
	active, child := makePair(false)
	for n := 0; n < 1200; n++ {
		parent, _ := makePair(true)
		historical[parent.ID] = true
	}
	unrelated, _ := ids.FromPayload(ids.TypeExecution, 99999999)
	addSubscription := func() {
		t.Helper()
		_, _, err = j.PutSubscription(ctx, subscription.Subscription{
			Origin: active.OriginHostID.String(), State: subscription.StateActive, ExpiresAt: time.Now().Add(time.Hour),
			Filter:      subscription.EventFilter{ExecutionIDs: []string{unrelated.String()}, Kinds: []string{"terminal"}},
			Destination: callback.Destination{Kind: callback.DestinationStdout},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n < 1024; n++ {
		addSubscription()
	}
	j.Close()
	check := func(t *testing.T, incomplete bool) {
		before, err := os.ReadFile(f.journal)
		if err != nil {
			t.Fatal(err)
		}
		var out, errors bytes.Buffer
		app := testApp(&out, &errors)
		if problem := app.inbox(ctx, output.Renderer{Mode: output.JSON, Writer: &out}, c, []string{"--limit", "200"}); problem != nil {
			t.Fatal(problem)
		}
		var envelope struct {
			Result struct {
				Executions []inboxExecution `json:"executions"`
				Total      int              `json:"total"`
			} `json:"result"`
		}
		if err = json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		for _, item := range envelope.Result.Executions {
			if historical[item.ID] {
				t.Fatalf("settled history consumed inbox page: %s", item.ID)
			}
		}
		if envelope.Result.Total != 1 || len(envelope.Result.Executions) != 1 || envelope.Result.Executions[0].ID != active.ID {
			t.Fatalf("active parent crowded out: total=%d items=%+v", envelope.Result.Total, envelope.Result.Executions)
		}
		item := envelope.Result.Executions[0]
		if incomplete {
			if item.ChildExitGuard == nil || item.ChildExitGuard.CheckError != "children_query_incomplete" {
				t.Fatalf("bounded active query silently settled: %+v", item)
			}
		} else if item.ChildExitGuard == nil || item.ChildExitGuard.CheckError != "" || len(item.UncollectedChildren) != 1 || item.UncollectedChildren[0].ID != child.ID {
			t.Fatalf("active obligation lost: %+v", item)
		}
		after, err := os.ReadFile(f.journal)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("inbox rewrote historical exit/outcome evidence")
		}
	}
	t.Run("1024-valid-subscriptions", func(t *testing.T) { check(t, false) })
	j, err = store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	addSubscription()
	j.Close()
	// Crossing the existing hard bound makes the regression deterministic even
	// on hardware fast enough not to exhaust the 1.5s budget with 1,024 records.
	// Only the active parent may report incomplete coverage; history is settled.
	t.Run("deterministic-subscription-bound", func(t *testing.T) { check(t, true) })
}

func TestInboxActiveChildrenRequireActualCollection(t *testing.T) {
	parent := func(n uint64) ids.ExecutionID {
		id, _ := ids.FromPayload(ids.TypeExecution, n)
		parsed, _ := ids.ParseExecutionID(id.String())
		return parsed
	}
	var children []model.Execution
	acks := store.AcknowledgementIndex{ByID: map[ids.ExecutionID]store.ExecutionAcknowledgement{}}
	cases := []struct {
		state                model.State
		ack, refused, active bool
	}{
		{model.StateRunning, true, false, true}, // Nonterminal acknowledgements cannot settle work.
		{model.StateAttention, true, false, true},
		{model.StateCompleted, false, false, true}, // Missing legacy epoch is not collection.
		{model.StateFailed, false, false, true},
		{model.StateCompleted, true, false, false},
		{model.StateCancelled, true, false, false},
		{model.StateCancelled, false, true, false},
	}
	for n, c := range cases {
		p, id := parent(uint64(100+n)), parent(uint64(1000+n))
		child := model.Execution{ID: id, ParentExecutionID: &p, State: c.state}
		if c.refused {
			s := admissionRefusedSource
			child.SourceState = &s
		}
		if c.ack {
			acks.ByID[id] = store.ExecutionAcknowledgement{ExecutionID: id}
		}
		children = append(children, child)
	}
	active, err := inboxActiveChildParents(context.Background(), children, acks)
	if err != nil {
		t.Fatal(err)
	}
	for n, c := range cases {
		if active[parent(uint64(100+n))] != c.active {
			t.Fatalf("case %d settled=%t, want active=%t", n, !active[parent(uint64(100+n))], c.active)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if active, err := inboxActiveChildParents(ctx, children, acks); err == nil || active != nil {
		t.Fatal("cancellation produced a partial settled set")
	}
}
