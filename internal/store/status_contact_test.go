package store

import (
	"context"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
)

func TestStatusContactDoesNotBumpRevision(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	created, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{Scope: "execution:create", Key: "status-contact", InputDigest: hash('c')})
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Minute)
	if err := journal.RecordStatusContact(ctx, created.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordStatusContact(ctx, created.ID, later.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	current, err := journal.GetExecution(ctx, created.ID)
	if err != nil || current.Revision != created.Revision {
		t.Fatalf("revision changed: %#v %v", current.Revision, err)
	}
	index, err := journal.StatusContactIndex(ctx)
	if err != nil || !index[created.ID].Equal(later) {
		t.Fatalf("contact=%v err=%v", index[created.ID], err)
	}
}
