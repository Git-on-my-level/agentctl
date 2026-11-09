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

// Policy reloads each maintenance pass; the kill switch needs no restart. No read-only command invokes this hook.
func (a *app) maintainJournal(ctx context.Context, c common, path string) error {
	return a.maintainJournalCursor(ctx, c, path, nil)
}

func (a *app) maintainJournalCursor(ctx context.Context, c common, path string, cursor *store.MaintenanceCursor) (err error) {
	defer func() {
		if err != nil && a.stderr != nil {
			fmt.Fprintf(a.stderr, "agentctl: journal retention deferred: %v\n", err)
		}
	}()
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
	links, err := a.anxRetentionLinks(c)
	if err != nil {
		return fmt.Errorf("journal retention ANX references: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cleanup := store.Cleanup{Open: func(ctx context.Context, readOnly bool) (*store.Journal, error) {
		return openJournalWithBusyBudget(ctx, path, store.Options{ReadOnly: readOnly}, true, 250*time.Millisecond)
	}}
	progress, err := cleanup.MaintainNext(ctx, a.now().UTC().Add(-age), cursor, links...)
	if err != nil {
		return fmt.Errorf("journal retention: %w", err)
	}
	if progress.Batches > 0 && a.stderr != nil {
		fmt.Fprintf(a.stderr, "agentctl: journal retention committed %d batch: removed %d acknowledged terminal executions and %d events; pages reusable\n", progress.Batches, progress.Executions, progress.Events)
	}
	return nil
}

// Rate-limit attempts, including failures. Preserve an unsuccessful pass in
// supervisor health until a later pass succeeds, instead of clearing it on the
// next five-second cycle that performs no maintenance.
type journalMaintenance struct {
	next      time.Time
	lastError error
	cursor    store.MaintenanceCursor
}

func (m *journalMaintenance) run(ctx context.Context, a *app, c common, path string) error {
	if a.now().Before(m.next) {
		return m.lastError
	}
	m.next = a.now().Add(5 * time.Minute)
	m.lastError = a.maintainJournalCursor(ctx, c, path, &m.cursor)
	return m.lastError
}
