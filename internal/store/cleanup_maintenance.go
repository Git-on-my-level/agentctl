package store

import (
	"context"
	"errors"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

// MaintenanceCursor is owned by one supervisor (not shared concurrently). It
// retains copied metadata and page bookmarks, never an open DB or transaction.
// Cancellation resumes at the last completed page. Any intervening journal
// commit invalidates the proof, including writes by older binaries.
type MaintenanceCursor struct {
	snapshot  *cleanupSnapshot
	bucket    int
	after     []byte
	path      string
	completed maintenanceCompletion
}

var cleanupProtectionBuckets = [][]byte{bMetadata, bExecutions, bSubscriptions, bOutbox, bOutboxDead, bOutboxReceipts, bAcknowledgements, bIdempotency, bANXRunLinks}

// MaintainNext resumes a protection scan, then commits at most one event batch.
// Busy/deadline errors preserve completed pages; conflicts reset the proof.
// A continuously mutating journal can defer retention: fail closed and expose
// that deferral to the operator rather than deleting from a torn graph.
func (c Cleanup) MaintainNext(ctx context.Context, before time.Time, cursor *MaintenanceCursor, links ...ids.ExecutionID) (progress CleanupProgress, err error) {
	if before.IsZero() {
		return progress, errors.New("cleanup cutoff is required")
	}
	if cursor == nil {
		cursor = &MaintenanceCursor{}
	}
	defer func() {
		if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrBusy)) {
			completed := cursor.completed
			if err == nil && cursor.snapshot != nil {
				completed = maintenanceCompletion{version: cursor.snapshot.version, path: cursor.path, links: maintenanceLinksKey(links), nextBefore: cursor.snapshot.nextBefore, nextCheckAt: cursor.snapshot.nextCheckAt}
			}
			if errors.Is(err, ErrConflict) {
				completed = maintenanceCompletion{}
			}
			*cursor = MaintenanceCursor{completed: completed}
		}
	}()
	r := c.reader(ctx)
	r.path = cursor.path
	if cursor.snapshot == nil && cursor.completed.version != 0 {
		r.path = cursor.completed.path
		skip, checkErr := r.maintenanceUnchanged(cursor.completed, before, links)
		if checkErr != nil {
			return progress, checkErr
		}
		if skip {
			return progress, nil
		}
	}
	defer func() {
		if cursor.snapshot != nil {
			cursor.path = r.path
		}
	}()
	if cursor.snapshot == nil {
		cursor.snapshot = &cleanupSnapshot{buckets: map[string]cleanupBucket{}}
	}
	s := cursor.snapshot
	s.ctx = ctx
	for cursor.bucket < len(cleanupProtectionBuckets) {
		name := cleanupProtectionBuckets[cursor.bucket]
		if s.Bucket(name) == nil {
			s.buckets[string(name)] = cleanupBucket{}
		}
		page, e := r.page(name, nil, cursor.after, &s.version)
		if e != nil {
			return progress, e
		}
		if len(page) == 0 {
			cursor.bucket++
			cursor.after = nil
			continue
		}
		for _, row := range page {
			s.Bucket(name)[string(row.key)] = row.value
		}
		cursor.after = page[len(page)-1].key
	}
	// Resolve only cursor/delivery event references. This step is also bounded
	// into pages by the common reader and checks the same generation.
	if err = r.snapshotEvents(s, true); err != nil {
		return progress, err
	}
	r.cachedSnapshot = s
	options := CleanupOptions{automatic: true, ANXRunLinks: links}
	_, targets, err := r.plan(before, options, cleanupEventBatch)
	if err != nil {
		return progress, err
	}
	return r.remove(before, options, targets, 1)
}
