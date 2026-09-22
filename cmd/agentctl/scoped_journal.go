package main

import (
	"context"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	agentruntime "github.com/Git-on-my-level/agentctl/internal/runtime"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// scopedJournal never retains a process-wide bbolt lock across adapter calls.
// Each mutation still uses the store's atomic CAS/event-deduplication contract.
// Engine reads are snapshots: a later mutation must tolerate a newer revision.
type scopedJournal struct{ path string }

func (s scopedJournal) with(ctx context.Context, readOnly bool, fn func(*store.Journal) error) error {
	j, err := openJournalWithRetryContext(ctx, s.path, store.Options{ReadOnly: readOnly})
	if err != nil {
		return err
	}
	defer j.Close()
	return fn(j)
}
func (s scopedJournal) HostID(ctx context.Context) (v ids.HostID, err error) {
	err = s.with(ctx, true, func(j *store.Journal) error { v, err = j.HostID(ctx); return err })
	return
}
func (s scopedJournal) GetExecution(ctx context.Context, id ids.ExecutionID) (v model.Execution, err error) {
	err = s.with(ctx, true, func(j *store.Journal) error { v, err = j.GetExecution(ctx, id); return err })
	return
}
func (s scopedJournal) ListExecutions(ctx context.Context, active bool) (v []model.Execution, err error) {
	err = s.with(ctx, true, func(j *store.Journal) error { v, err = j.ListExecutions(ctx, active); return err })
	return
}
func (s scopedJournal) ListEvents(ctx context.Context, id ids.ExecutionID, q contracts.EventQuery) (v []model.Event, err error) {
	err = s.with(ctx, true, func(j *store.Journal) error { v, err = j.ListEvents(ctx, id, q); return err })
	return
}
func (s scopedJournal) CreateExecution(ctx context.Context, e model.Execution, k contracts.MutationKey) (v model.Execution, reused bool, err error) {
	err = s.with(ctx, false, func(j *store.Journal) error { v, reused, err = j.CreateExecution(ctx, e, k); return err })
	return
}
func (s scopedJournal) UpdateExecution(ctx context.Context, e model.Execution, revision uint64) (v model.Execution, err error) {
	err = s.with(ctx, false, func(j *store.Journal) error { v, err = j.UpdateExecution(ctx, e, revision); return err })
	return
}
func (s scopedJournal) AppendEvent(ctx context.Context, e model.Event, raw []byte) (v model.Event, reused bool, err error) {
	err = s.with(ctx, false, func(j *store.Journal) error { v, reused, err = j.AppendEvent(ctx, e, raw); return err })
	return
}

var _ agentruntime.Journal = scopedJournal{}

func (s scopedJournal) GetExecutionByMutation(ctx context.Context, k contracts.MutationKey) (v model.Execution, found bool, err error) {
	err = s.with(ctx, true, func(j *store.Journal) error { v, found, err = j.GetExecutionByMutation(ctx, k); return err })
	return
}
