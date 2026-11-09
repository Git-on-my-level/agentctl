package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/store"
)

const journalRetentionAge = 30 * 24 * time.Hour

// Only the explicitly running supervisor performs retention. Read-only CLI
// commands never maintain the journal as a side effect.
func maintainJournal(ctx context.Context, path string, now time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	read, err := openJournalWithBusyBudget(ctx, path, store.Options{ReadOnly: true}, true, 250*time.Millisecond)
	if err != nil {
		return 0, err
	}
	batch, err := read.PlanCleanupBatch(ctx, now.Add(-journalRetentionAge))
	read.Close()
	if err != nil || batch.Executions() == 0 {
		return 0, err
	}
	write, err := openJournalWithBusyBudget(ctx, path, store.Options{}, true, 250*time.Millisecond)
	if err != nil {
		return 0, err
	}
	defer write.Close()
	return write.ApplyCleanupBatch(ctx, batch)
}

func journalMaintenanceLog(w io.Writer, removed int, err error) {
	if w == nil {
		return
	}
	// Contention and generation changes defer one batch; they are not evidence
	// of corruption or a failed execution. Never extend the recovery lock budget.
	if errors.Is(err, store.ErrBusy) || errors.Is(err, store.ErrConflict) {
		return
	}
	if err != nil {
		fmt.Fprintf(w, "agentctl: journal retention deferred: %v\n", err)
	} else if removed > 0 {
		fmt.Fprintf(w, "agentctl: journal retention removed %d terminal executions; pages reusable\n", removed)
	}
}
