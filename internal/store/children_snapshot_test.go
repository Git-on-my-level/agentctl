package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

func TestChildMembershipSnapshotRequiresCompleteStableEvidence(t *testing.T) {
	ctx := context.Background()
	j, child, _, _ := childrenCorruptionFixture(t)
	parent := *child.ParentExecutionID
	parents := []ids.ExecutionID{parent}
	snapshot, err := j.ListExecutions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	// A writable handle cannot guarantee that an earlier projection snapshot
	// remains stable while index evidence is read in another transaction.
	if err := j.ChildMembershipErrors(ctx, parents, snapshot)[parent]; !errors.Is(err, ErrConflict) {
		t.Fatalf("writable snapshot certified: %v", err)
	}
	path := j.path
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = reader.ListExecutions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.ChildMembershipErrors(ctx, parents, snapshot)[parent]; err != nil {
		t.Fatal(err)
	}
	var partial []model.Execution
	for _, e := range snapshot {
		if e.ID != child.ID {
			partial = append(partial, e)
		}
	}
	if err := reader.ChildMembershipErrors(ctx, parents, partial)[parent]; !errors.Is(err, ErrChildrenBounded) {
		t.Fatalf("partial snapshot falsely certified: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := reader.ChildMembershipErrors(cancelled, parents, snapshot)[parent]; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled proof certified: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("proof mutated journal: %v", err)
	}
}
