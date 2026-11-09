package store

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/diagnostic"
	"go.etcd.io/bbolt"
)

// CleanupBatch is an opaque, process-local plan. It cannot be replayed against
// another journal or after a concurrent commit. No prompts/results leave it.
type CleanupBatch struct {
	path    string
	txID    int
	targets []cleanupTarget
	plan    CleanupPlan
}

func (b CleanupBatch) Executions() int { return len(b.targets) }

// PlanCleanupBatch retains the normal graph, delivery, diagnostic and unread
// result protections. Whole components exceeding a batch are deferred to the
// explicit cleanup command. Planning is cancellable and never writes.
func (j *Journal) PlanCleanupBatch(ctx context.Context, before time.Time) (CleanupBatch, error) {
	batch := CleanupBatch{path: j.path}
	if err := ctx.Err(); err != nil {
		return batch, err
	}
	if before.IsZero() {
		return batch, fmt.Errorf("cleanup cutoff is required")
	}
	err := j.db.View(func(tx *bbolt.Tx) error {
		batch.txID = tx.ID()
		var err error
		batch.plan, batch.targets, err = j.cleanupPlanTx(ctx, tx, before.UTC(), CleanupOptions{maxExecutions: 32, maxEvents: 4096})
		return err
	})
	return batch, err
}

// ApplyCleanupBatch does no history scan under the exclusive handle. The read
// plan's generation must still match, otherwise nothing is touched. The caller
// must close the planning handle before opening this writable handle. bbolt's
// inode and schema-1 format remain compatible with older binaries.
func (j *Journal) ApplyCleanupBatch(ctx context.Context, batch CleanupBatch) (int, error) {
	if j.readOnly {
		return 0, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if batch.path != j.path || batch.txID == 0 {
		return 0, ErrConflict
	}
	if len(batch.targets) == 0 {
		return 0, nil
	}
	outputs, err := diagnostic.OpenOutputs(filepath.Dir(j.path), true)
	if err != nil {
		return 0, err
	}
	defer outputs.Close()
	err = j.db.Update(func(tx *bbolt.Tx) error {
		if tx.ID() != batch.txID+1 {
			return ErrConflict
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, target := range batch.targets {
			if target.DiagnosticFile != nil {
				if err := outputs.Remove(*target.DiagnosticFile); err != nil {
					return err
				}
			}
		}
		return deleteCleanupTargets(ctx, tx, batch.targets)
	})
	if err != nil {
		return 0, err
	}
	return len(batch.targets), nil
}
