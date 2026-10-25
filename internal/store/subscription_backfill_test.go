package store

import (
	"context"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

// A subscription created moments after its execution ended must still
// receive the terminal event: fan-out only runs on append, so without a
// backfill such a subscription waited forever (seen live on 2026-10-24).
func TestExactExecutionSubscriptionBackfillsRecordedTerminal(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	finished := sampleExecution(now)
	finished.Labels = []string{"lane.done"}
	finished, _, err := j.CreateExecution(ctx, finished, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	terminalState := model.StateCompleted
	key, projection := semanticEvent(t, finished.Adapter, map[string]any{"terminal": "backfill"})
	terminal, _, err := j.AppendEvent(ctx, model.Event{ExecutionID: finished.ID, Authority: finished.Authority, Adapter: finished.Adapter, Ordering: model.OrderingObservation, Kind: model.EventTerminal, State: &terminalState, ObservedAt: now, DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{"status": "completed"}}, projection)
	if err != nil {
		t.Fatal(err)
	}

	put := func(payload uint64, filter subscription.EventFilter) string {
		t.Helper()
		value := durableTestSubscription(durableTestID(ids.TypeSubscription, payload))
		value.Filter = filter
		created, _, err := j.PutSubscription(ctx, value)
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	exact := put(901, subscription.EventFilter{ExecutionIDs: []string{finished.ID.String()}, Kinds: []string{string(model.EventTerminal)}})
	attentionOnly := put(902, subscription.EventFilter{ExecutionIDs: []string{finished.ID.String()}, Kinds: []string{string(model.EventAttention)}})
	stillRunning := put(903, subscription.EventFilter{ExecutionIDs: []string{running.ID.String()}, Kinds: []string{string(model.EventTerminal)}})
	labelScope := put(904, subscription.EventFilter{LabelPrefix: "lane.", Kinds: []string{string(model.EventTerminal)}})

	pending, err := j.ListPendingDeliveries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SubscriptionID != exact || pending[0].ExecutionID != finished.ID.String() || pending[0].EventID != terminal.ID.String() {
		t.Fatalf("backfilled deliveries=%#v; want exactly the terminal for %s (not %s, %s, %s)", pending, exact, attentionOnly, stillRunning, labelScope)
	}
	// Re-creating the identical subscription is idempotent and does not
	// enqueue a second delivery.
	value := durableTestSubscription(exact)
	value.Filter = subscription.EventFilter{ExecutionIDs: []string{finished.ID.String()}, Kinds: []string{string(model.EventTerminal)}}
	if _, reused, err := j.PutSubscription(ctx, value); err != nil || !reused {
		t.Fatalf("re-put reused=%v err=%v", reused, err)
	}
	if again, err := j.ListPendingDeliveries(ctx); err != nil || len(again) != 1 {
		t.Fatalf("re-put deliveries=%d err=%v", len(again), err)
	}
}
