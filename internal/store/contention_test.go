package store

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
	"go.etcd.io/bbolt"
)

// Only the sequence index needs a long history to measure append's lock cost.
// Seed it in one transaction rather than timing thousands of fsyncs as setup.
func BenchmarkAppendWithHistory(b *testing.B) {
	for _, history := range []int{100, 100000} {
		b.Run(fmt.Sprint(history), func(b *testing.B) {
			path := filepath.Join(testutil.TempDir(b), "state", "journal.db")
			j, err := Open(path, Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer j.Close()
			e, _, err := j.CreateExecution(context.Background(), sampleExecution(time.Now()), contracts.MutationKey{})
			if err != nil {
				b.Fatal(err)
			}
			err = j.db.Update(func(tx *bbolt.Tx) error {
				bucket, err := tx.Bucket(bEventsByExecution).CreateBucketIfNotExists([]byte(e.ID.String()))
				if err != nil {
					return err
				}
				for i := 1; i <= history; i++ {
					var key [8]byte
					binary.BigEndian.PutUint64(key[:], uint64(i))
					if err := bucket.Put(key[:], []byte("fixture")); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key, canonical, err := callback.SemanticDedupeKey(e.Adapter, 1, map[string]any{"i": i})
				if err != nil {
					b.Fatal(err)
				}
				event := model.Event{ExecutionID: e.ID, OriginHostID: e.OriginHostID, Authority: e.Authority, Adapter: e.Adapter, Kind: model.EventProgress, Ordering: model.OrderingObservation, DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{}}
				stored, _, err := j.AppendEvent(context.Background(), event, canonical)
				if err != nil || stored.Sequence != uint64(history+i+1) {
					b.Fatalf("sequence=%d err=%v", stored.Sequence, err)
				}
			}
		})
	}
}

func TestReopeningInitializedJournalDoesNotCommit(t *testing.T) {
	j, _, _ := openTestJournal(t)
	var before bbolt.TxStats
	var txid int
	if err := j.db.View(func(tx *bbolt.Tx) error { txid = tx.ID(); before = tx.Stats(); return nil }); err != nil {
		t.Fatal(err)
	}
	path := j.Path()
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := next.db.View(func(tx *bbolt.Tx) error {
		if tx.ID() != txid {
			t.Fatalf("writable reopen committed metadata: txid %d -> %d (prior stats %+v)", txid, tx.ID(), before)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLastEventSequenceUsesIndexAndPreservesGaps(t *testing.T) {
	j, _, now := openTestJournal(t)
	e, _, err := j.CreateExecution(context.Background(), sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	last, err := j.LastEventSequence(context.Background(), e.ID)
	if err != nil || last != 0 {
		t.Fatalf("empty sequence=%d err=%v", last, err)
	}
	key, projection := semanticEvent(t, e.Adapter, map[string]any{"i": 1})
	event, _, err := j.AppendEvent(context.Background(), model.Event{ExecutionID: e.ID, OriginHostID: e.OriginHostID, Authority: e.Authority, Adapter: e.Adapter, Kind: model.EventProgress, Ordering: model.OrderingObservation, DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{}}, projection)
	if err != nil {
		t.Fatal(err)
	}
	err = j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bEventsByExecution).Bucket([]byte(e.ID.String()))
		var first, last [8]byte
		binary.BigEndian.PutUint64(first[:], 1)
		binary.BigEndian.PutUint64(last[:], 100000)
		if err := bucket.Delete(first[:]); err != nil {
			return err
		}
		return bucket.Put(last[:], []byte(event.ID.String()))
	})
	if err != nil {
		t.Fatal(err)
	}
	last, err = j.LastEventSequence(context.Background(), e.ID)
	if err != nil || last != 100000 {
		t.Fatalf("gapped sequence=%d err=%v", last, err)
	}
	key, projection = semanticEvent(t, e.Adapter, map[string]any{"i": 2})
	event.ID = ""
	event.DedupeKey = key
	next, _, err := j.AppendEvent(context.Background(), event, projection)
	if err != nil || next.Sequence != 100001 {
		t.Fatalf("append sequence=%d err=%v", next.Sequence, err)
	}
}

func BenchmarkSteeringSequenceHistory(b *testing.B) {
	path := filepath.Join(testutil.TempDir(b), "state", "journal.db")
	j, err := Open(path, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer j.Close()
	e, _, err := j.CreateExecution(context.Background(), sampleExecution(time.Now()), contracts.MutationKey{})
	if err != nil {
		b.Fatal(err)
	}
	err = j.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.Bucket(bEventsByExecution).CreateBucketIfNotExists([]byte(e.ID.String()))
		if err != nil {
			return err
		}
		for i := 1; i <= 10000; i++ {
			eid, _ := ids.FromPayload(ids.TypeEvent, uint64(i))
			eventID, _ := ids.ParseEventID(eid.String())
			event := model.Event{SchemaVersion: model.SchemaVersion, ID: eventID, ExecutionID: e.ID, OriginHostID: e.OriginHostID, Authority: e.Authority, Adapter: e.Adapter, Ordering: model.OrderingObservation, DedupeKey: hash('a'), DedupeVersion: 1, ObservedAt: time.Now().UTC(), Sequence: uint64(i), Kind: model.EventProgress, Payload: map[string]any{"type": "fixture"}}
			body, err := json.Marshal(storedEvent{Event: event})
			if err != nil {
				return err
			}
			if err := tx.Bucket(bEvents).Put([]byte(eid.String()), body); err != nil {
				return err
			}
			var sequence [8]byte
			binary.BigEndian.PutUint64(sequence[:], uint64(i))
			if err := bucket.Put(sequence[:], []byte(eid.String())); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, kind := range []string{"legacy_decode_all", "index_last"} {
		b.Run(kind, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var sequence uint64
				if kind == "index_last" {
					sequence, err = j.LastEventSequence(context.Background(), e.ID)
				} else {
					for {
						var events []model.Event
						events, err = j.ListEvents(context.Background(), e.ID, contracts.EventQuery{AfterSequence: sequence, Limit: 1000})
						if err != nil || len(events) == 0 {
							break
						}
						sequence = events[len(events)-1].Sequence
					}
				}
				if err != nil || sequence != 10000 {
					b.Fatalf("sequence=%d err=%v", sequence, err)
				}
			}
		})
	}
}

// legacy_metadata_commit reproduces the unconditional metadata Put removed
// from initialize. Both cases use the same durable database and open/close path.
func BenchmarkWritableReopen(b *testing.B) {
	path := filepath.Join(testutil.TempDir(b), "state", "journal.db")
	j, err := Open(path, Options{})
	if err != nil {
		b.Fatal(err)
	}
	j.Close()
	for _, kind := range []string{"legacy_metadata_commit", "initialized_read"} {
		b.Run(kind, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				j, err := Open(path, Options{})
				if err != nil {
					b.Fatal(err)
				}
				if kind == "legacy_metadata_commit" {
					err = j.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bMetadata).Put(keySchema, []byte("1")) })
				}
				j.Close()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestFilteredExecutionsPreserveUnappliedTerminalEvents(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := sampleExecution(now)
	e.Labels = []string{"live.lane"}
	e, _, err := j.CreateExecution(ctx, e, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	key, projection := semanticEvent(t, e.Adapter, map[string]any{"unapplied_terminal": true})
	state := model.StateCompleted
	if _, _, err = j.AppendEvent(ctx, model.Event{ExecutionID: e.ID, Authority: e.Authority, Adapter: e.Adapter, Kind: model.EventTerminal, State: &state, Ordering: model.OrderingObservation, DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{}}, projection); err != nil {
		t.Fatal(err)
	}
	seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	active, err := j.ListExecutions(ctx, true)
	if err != nil || len(active) != 1 || active[0].ID != e.ID {
		t.Fatalf("unapplied terminal hidden: %v %v", active, err)
	}
	labeled, err := j.ListExecutionsByLabelPrefix(ctx, "live.")
	if err != nil || len(labeled) != 1 || labeled[0].ID != e.ID {
		t.Fatalf("label selection: %v %v", labeled, err)
	}
}
