package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

func TestAcknowledgementIndexTreatsPreEpochTerminalsAsReconciled(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	past := now.Add(-2 * time.Hour)
	legacy := sampleExecution(past)
	legacy.CreatedAt = past
	legacy.UpdatedAt = past
	legacy.Observation.ObservedAt = past
	legacy.Capabilities.NegotiatedAt = past
	legacy.State = model.StateCompleted
	legacy.Liveness = model.LivenessExited
	legacy.TerminalAt = &past
	created, _, err := journal.CreateExecution(ctx, legacy, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	index, err := journal.AcknowledgementIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if index.Epoch.IsZero() || index.Unreconciled(created) {
		t.Fatalf("legacy terminal before epoch should be reconciled: epoch=%s unreconciled=%v", index.Epoch, index.Unreconciled(created))
	}

	running, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if index.Unreconciled(running) {
		t.Fatal("nonterminal execution was unreconciled")
	}

	terminalAt := now.Add(time.Minute)
	next := running
	next.State = model.StateCompleted
	next.Liveness = model.LivenessExited
	next.TerminalAt = &terminalAt
	next.UpdatedAt = terminalAt
	next.Observation.ObservedAt = terminalAt
	updated, err := journal.UpdateExecution(ctx, next, running.Revision)
	if err != nil {
		t.Fatal(err)
	}
	index, err = journal.AcknowledgementIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !index.Unreconciled(updated) {
		t.Fatal("post-epoch terminal without a stamp should be unreconciled")
	}

	ack, reused, err := journal.AcknowledgeExecution(ctx, updated.ID, AcknowledgementResult)
	if err != nil || reused || ack.Source != AcknowledgementResult || ack.AcknowledgedAt.IsZero() {
		t.Fatalf("ack=%#v reused=%v err=%v", ack, reused, err)
	}
	again, reused, err := journal.AcknowledgeExecution(ctx, updated.ID, AcknowledgementAwait)
	if err != nil || !reused || again.AcknowledgedAt != ack.AcknowledgedAt || again.Source != AcknowledgementResult {
		t.Fatalf("idempotent ack changed stamp: %#v reused=%v err=%v", again, reused, err)
	}
	index, err = journal.AcknowledgementIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if index.Unreconciled(updated) {
		t.Fatal("acknowledged execution remained unreconciled")
	}
}

func TestAcknowledgementIndexWithoutEpochHidesHistoricalWork(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	terminalAt := now
	execution := sampleExecution(now)
	execution.State = model.StateCompleted
	execution.Liveness = model.LivenessExited
	execution.TerminalAt = &terminalAt
	created, _, err := journal.CreateExecution(ctx, execution, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bMetadata).Delete(keyAckEpoch)
	}); err != nil {
		t.Fatal(err)
	}
	index, err := journal.AcknowledgementIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !index.Epoch.IsZero() || index.Unreconciled(created) {
		t.Fatalf("missing epoch should hide unstamped history: %#v unreconciled=%v", index.Epoch, index.Unreconciled(created))
	}
}

func TestAcknowledgeExecutionRejectsNonterminal(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	created, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := journal.AcknowledgeExecution(ctx, created.ID, AcknowledgementResult); !errors.Is(err, ErrConflict) {
		t.Fatalf("nonterminal ack err=%v", err)
	}
}

func TestCleanupRemovesExecutionAcknowledgement(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, journal, now.Add(-72*time.Hour), contracts.MutationKey{})
	if _, _, err := journal.AcknowledgeExecution(ctx, old.ID, AcknowledgementAwait); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	plan, err := journal.PlanCleanup(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 1 || plan.Records.Acknowledgements != 1 {
		t.Fatalf("plan missing acknowledgement: %#v", plan)
	}
	if _, err := journal.ApplyCleanup(ctx, cutoff, plan.PlanDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.GetAcknowledgement(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("acknowledgement survived cleanup: %v", err)
	}
}
