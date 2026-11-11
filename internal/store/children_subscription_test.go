package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
	"go.etcd.io/bbolt"
)

func TestChildrenSubscriptionFanoutParity(t *testing.T) {
	for _, test := range []struct {
		name       string
		corrupt    bool
		matching   bool // also store an independent valid matching subscription
		deliveries int
	}{
		{"valid-exact", false, false, 1},
		{"valid-unrelated", false, false, 0},
		{"invalid-id", true, false, 0},
		{"wrong-valid-id", true, false, 0},
		{"invalid-key", true, false, 0},
		{"invalid-origin", true, false, 0},
		{"invalid-filter-id", true, false, 0},
		{"invalid-coordinator-id", true, false, 0},
		{"invalid-cursor-id", true, false, 0},
		{"malformed-json", true, false, 0},
		{"malformed-record", true, false, 0},
		{"unrelated-invalid-id", true, true, 0},
		{"inactive-malformed-json", true, true, 0},
		{"inactive-invalid-id", false, true, 1},
		{"unknown-filter-invalid-id", false, true, 1},
		{"unknown-filter", false, false, 0},
		{"unknown-destination", false, false, 0},
		{"paused", false, false, 0},
		{"expired", false, false, 0},
		{"null", false, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			j, _, now := openTestJournal(t)
			parent, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
			if err != nil {
				t.Fatal(err)
			}
			e := sampleExecution(now)
			e.ParentExecutionID = &parent.ID
			e.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "explicit"}
			e, _, err = j.CreateExecution(ctx, e, contracts.MutationKey{})
			if err != nil {
				t.Fatal(err)
			}
			value := durableTestSubscription(durableTestID(ids.TypeSubscription, 2))
			value.Filter.ExecutionIDs = []string{e.ID.String()}
			value.Filter.Kinds = []string{"terminal"}
			key := []byte(value.ID)
			switch test.name {
			case "valid-unrelated", "unrelated-invalid-id":
				value.Filter.ExecutionIDs = []string{parent.ID.String()}
			case "invalid-origin":
				value.Origin = "invalid-host"
			case "invalid-filter-id":
				value.Filter.ExecutionIDs = append(value.Filter.ExecutionIDs, "invalid-execution")
			case "invalid-coordinator-id":
				value.CoordinatorExecutionID = "invalid-execution"
			case "invalid-cursor-id":
				value.Cursor.ID = "invalid-cursor"
			case "wrong-valid-id":
				value.ID = durableTestID(ids.TypeSubscription, 3)
			case "invalid-key":
				key = []byte("invalid-key")
			case "paused", "inactive-invalid-id", "inactive-malformed-json":
				value.State = subscription.StatePaused
			case "expired":
				value.ExpiresAt = now.Add(-time.Hour)
			case "unknown-destination":
				value.Destination.Kind = "future-transport"
			}
			switch test.name {
			case "invalid-id", "unrelated-invalid-id", "inactive-invalid-id", "unknown-filter-invalid-id":
				value.ID = "invalid-subscription"
			}
			raw, err := json.Marshal(SubscriptionRecord{Subscription: value, Revision: 1, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			switch test.name {
			case "malformed-json", "inactive-malformed-json":
				raw = []byte(`{"subscription":{"state":"paused"},`)
			case "malformed-record":
				raw = []byte(`{"subscription":[]}`)
			case "unknown-filter", "unknown-filter-invalid-id":
				raw = bytes.Replace(raw, []byte(`"filter":{`), []byte(`"filter":{"future_scope":"narrow",`), 1)
			case "null":
				raw = []byte(`null`)
			}
			if test.matching {
				known := durableTestSubscription(durableTestID(ids.TypeSubscription, 1))
				known.Filter.ExecutionIDs, known.Filter.Kinds = []string{e.ID.String()}, []string{"terminal"}
				if _, _, err := j.PutSubscription(ctx, known); err != nil {
					t.Fatal(err)
				}
			}
			if err := j.db.Update(func(tx *bbolt.Tx) error {
				b, err := tx.CreateBucketIfNotExists(bSubscriptions)
				if err != nil {
					return err
				}
				return b.Put(key, raw)
			}); err != nil {
				t.Fatal(err)
			}
			// Exercise real fanout, including rollback after an earlier valid match.
			// Outbox metadata is inspected only; no notification worker is run.
			state := model.StateCompleted
			event := model.Event{SchemaVersion: model.SchemaVersion, ID: ids.EventID(durableTestID(ids.TypeEvent, 900)), OriginHostID: e.OriginHostID, ExecutionID: e.ID, Sequence: 1, Ordering: model.OrderingObservation, Kind: model.EventTerminal, State: &state, Authority: e.Authority, Adapter: e.Adapter, ObservedAt: now, DedupeKey: hash('b'), DedupeVersion: 1, Payload: map[string]any{"status": "completed"}}
			err = j.db.Update(func(tx *bbolt.Tx) error {
				return enqueueMatchingSubscriptionsTx(tx, &event, nil, now, j.generator)
			})
			if test.corrupt {
				if !errors.Is(err, ErrCorrupt) {
					t.Fatalf("fanout error=%v, want ErrCorrupt", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			deliveries, err := j.ListPendingDeliveries(ctx)
			if err != nil || len(deliveries) != test.deliveries {
				t.Fatalf("fanout deliveries=%d error=%v, want %d", len(deliveries), err, test.deliveries)
			}
			var wantErr error
			wantCount := 1
			if test.corrupt {
				wantErr = ErrCorrupt
			} else if test.deliveries > 0 {
				wantCount = 0
			}
			checkChildrenCorruptionRead(t, j, []ChildrenScope{{Parent: &parent.ID}, {Provider: e.CallerIdentity.Provider, NativeSessionID: e.CallerIdentity.NativeSessionID}}, now, wantErr, wantCount)
		})
	}
}
