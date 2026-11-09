package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// Policy is reloaded every cycle, so the kill switch takes effect without a
// supervisor restart. No read-only command invokes this hook.
func (a *app) maintainJournal(ctx context.Context, c common, path string) error {
	var policy *config.Journal
	resolution, err := configResolution(c)
	if err == nil {
		policy = resolution.Config.Journal
	} else if !errors.Is(err, config.ErrNotFound) {
		return fmt.Errorf("journal retention config: %w", err)
	}
	enabled, age := policy.RetentionPolicy()
	if !enabled {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cleanup := store.Cleanup{Open: func(ctx context.Context, readOnly bool) (*store.Journal, error) {
		return openJournalWithBusyBudget(ctx, path, store.Options{ReadOnly: readOnly}, true, 250*time.Millisecond)
	}}
	progress, err := cleanup.Maintain(ctx, a.now().UTC().Add(-age))
	if errors.Is(err, store.ErrBusy) || errors.Is(err, store.ErrConflict) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("journal retention: %w", err)
	}
	if progress.Batches > 0 && a.stderr != nil {
		fmt.Fprintf(a.stderr, "agentctl: journal retention committed %d batch: removed %d acknowledged terminal executions and %d events; pages reusable\n", progress.Batches, progress.Executions, progress.Events)
	}
	return nil
}
