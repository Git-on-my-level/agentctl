package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

func TestInventoryAndRecoveryReleaseFileLockBetweenPages(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	for i := 0; i < cleanupPageSize+3; i++ {
		seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	}
	running, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := j.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	opens := 0
	reader := Reader{Open: func(ctx context.Context, ro bool) (*Journal, error) {
		if !ro {
			t.Fatal("read command requested a write handle")
		}
		writer, err := Open(path, Options{LockTimeout: time.Millisecond})
		if err != nil {
			t.Fatal("read retained flock", err)
		}
		writer.Close()
		opens++
		return Open(path, Options{ReadOnly: ro})
	}}
	before, _ := InventoryFileState(path)
	got, err := reader.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got.GeneratedAt = want.GeneratedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged inventory mismatch:\ngot=%+v\nwant=%+v", got, want)
	}
	active, err := reader.ListExecutions(ctx, true)
	if err != nil || len(active) != 1 || active[0].ID != running.ID {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	all, err := reader.ListExecutions(ctx, false)
	if err != nil || len(all) != cleanupPageSize+4 {
		t.Fatalf("listing=%d err=%v", len(all), err)
	}
	after, _ := InventoryFileState(path)
	if before != after || opens < 10 {
		t.Fatalf("mutation or unpaged reads: opens=%d %v %v", opens, before, after)
	}
}

func TestExecutionFiltersPrecedeBindingHydration(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	terminal := seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	initial := sampleExecution(now)
	initial.Labels = []string{"watched.lane"}
	running, _, err := j.CreateExecution(ctx, initial, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	// A corrupt immutable snapshot on an excluded terminal must not make active
	// recovery or a disjoint label query hydrate that record. Selected corruption
	// is still surfaced by the complete listing.
	if err = j.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bIdempotency).Put([]byte(delegationBindingScope+"\x00"+terminal.ID.String()), []byte(`{}`))
	}); err != nil {
		t.Fatal(err)
	}
	active, err := j.ListExecutions(ctx, true)
	if err != nil || len(active) != 1 || active[0].ID != running.ID {
		t.Fatalf("active=%v %v", active, err)
	}
	labeled, err := j.ListExecutionsByLabelPrefix(ctx, "watched.")
	if err != nil || len(labeled) != 1 || labeled[0].ID != running.ID {
		t.Fatalf("labels=%v %v", labeled, err)
	}
	if _, err = j.ListExecutions(ctx, false); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("selected corruption hidden: %v", err)
	}
	path := j.path
	j.Close()
	matched := 0
	if err := cleanupForPath(path).WalkExecutionsByLabelPrefix(ctx, "watched.", func(_ *Journal, page []model.Execution) error { matched += len(page); return nil }); err != nil || matched != 1 {
		t.Fatalf("paged label filtering hydrated excluded corruption: %d %v", matched, err)
	}
	active, err = cleanupForPath(path).ListExecutions(ctx, true)
	if err != nil || len(active) != 1 || active[0].ID != running.ID {
		t.Fatalf("paged active=%v %v", active, err)
	}
}

func TestPagedInventoryAndListingCancelBetweenPages(t *testing.T) {
	j, _, now := openTestJournal(t)
	for i := 0; i < cleanupPageSize+1; i++ {
		seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	}
	path := j.path
	j.Close()
	for _, inventory := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		opens := 0
		reader := Reader{Open: func(ctx context.Context, ro bool) (*Journal, error) {
			opens++
			db, err := Open(path, Options{ReadOnly: ro})
			if opens == 1 {
				cancel()
			}
			return db, err
		}}
		var err error
		if inventory {
			_, err = reader.Inventory(ctx)
		} else {
			_, err = reader.ListExecutions(ctx, false)
		}
		cancel()
		if !errors.Is(err, context.Canceled) || opens != 1 {
			t.Fatalf("inventory=%v err=%v opens=%d", inventory, err, opens)
		}
	}
}
