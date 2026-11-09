package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/diagnostic"
	"go.etcd.io/bbolt"
)

// CleanupProgress describes durable progress, including when cancellation,
// contention or a changed protection graph interrupts a multi-commit apply.
type CleanupProgress struct {
	Batches    int `json:"batches"`
	Events     int `json:"events"`
	Executions int `json:"executions"`
}
type CleanupInterrupted struct {
	Progress CleanupProgress
	Err      error
}

func (e *CleanupInterrupted) Error() string {
	return fmt.Sprintf("cleanup stopped after %d committed batches (%d executions, %d events); re-plan before retrying: %v", e.Progress.Batches, e.Progress.Executions, e.Progress.Events, e.Err)
}
func (e *CleanupInterrupted) Unwrap() error { return e.Err }

// ApplyCleanup retains the caller-owned handle; CLI paths use Cleanup.Apply
// to release bbolt file locks between every commit.
func (j *Journal) ApplyCleanup(ctx context.Context, before time.Time, digest string, options CleanupOptions) (CleanupPlan, error) {
	if j.readOnly {
		return CleanupPlan{}, ErrReadOnly
	}
	return j.cleanupReader(ctx).apply(before, digest, options)
}

// Apply validates the reviewed batch, then commits bounded prefixes. Errors
// after any commit report CleanupInterrupted and require a fresh plan.
func (c Cleanup) Apply(ctx context.Context, before time.Time, digest string, options CleanupOptions) (CleanupPlan, error) {
	return c.reader(ctx).apply(before, digest, options)
}

func (r *cleanupReader) apply(before time.Time, digest string, options CleanupOptions) (CleanupPlan, error) {
	if digest == "" {
		return CleanupPlan{}, fmt.Errorf("%w: cleanup plan digest is required", ErrConflict)
	}
	plan, targets, err := r.plan(before, options, cleanupEventBatch)
	if err != nil {
		return plan, err
	}
	if plan.PlanDigest != digest {
		return plan, fmt.Errorf("%w: cleanup plan changed; reviewed %s, current %s", ErrConflict, digest, plan.PlanDigest)
	}
	progress, err := r.remove(before, options, targets, 0)
	if err != nil {
		return plan, &CleanupInterrupted{progress, err}
	}
	plan.Applied = true
	return plan, nil
}

// Maintain performs at most one small event batch per cycle. Unlike manual
// cleanup it requires a real acknowledgement even for pre-tracking terminals.
// Event-heavy executions drain over successive cycles without losing their
// outcome, terminal event, acknowledgement or identity in between.
func (c Cleanup) Maintain(ctx context.Context, before time.Time) (CleanupProgress, error) {
	r := c.reader(ctx)
	options := CleanupOptions{automatic: true}
	_, targets, err := r.plan(before, options, cleanupEventBatch)
	if err != nil {
		return CleanupProgress{}, err
	}
	return r.remove(before, options, targets, 1)
}

func (r *cleanupReader) remove(before time.Time, options CleanupOptions, targets []cleanupTarget, maxBatches int) (progress CleanupProgress, err error) {
	for _, target := range targets {
		events := target.events
		for {
			if err := r.ctx.Err(); err != nil {
				return progress, err
			}
			snapshot, err := r.snapshot(true)
			if err != nil {
				return progress, err
			}
			_, eligible, err := snapshot.selectCleanup(before.UTC(), r.now, options)
			if err != nil {
				return progress, err
			}
			allowed := false
			for _, execution := range eligible {
				if execution.ID.String() == target.ExecutionID {
					allowed = true
					break
				}
			}
			if !allowed {
				return progress, fmt.Errorf("%w: execution %s acquired a cleanup protection", ErrConflict, target.ExecutionID)
			}
			n := min(len(events), cleanupEventBatch)
			size := 0
			for i, event := range events[:n] {
				size += len(event.raw) + len(event.dedupeRaw)
				if size > cleanupPageBytes && i > 0 {
					n = i
					break
				}
			}
			// Terminal and high-watermark anchors commit with the identity, never
			// in an intermediate prefix split by the byte budget.
			if target.complete && n < len(events) && n > len(events)-target.anchorCount {
				n = len(events) - target.anchorCount
				if n == 0 {
					n = len(events)
				}
			}
			final := target.complete && n == len(events)
			// Mutation keys are normally one or two records. Keep even an unusual
			// imported execution bounded; defer it rather than perform an unbounded
			// final transaction or remove its identity before all indexes are gone.
			if final && len(target.rawRecords) > cleanupPageSize {
				return progress, fmt.Errorf("%w: execution has too many cleanup metadata records", ErrConflict)
			}
			if final {
				mutations := 0
				if err := snapshot.forEach(bIdempotency, func(_, raw []byte) error {
					var record mutationRecord
					if err := json.Unmarshal(raw, &record); err != nil {
						return corrupt(err)
					}
					if record.ObjectType == "execution" && record.ObjectID == target.ExecutionID {
						mutations++
					}
					return nil
				}); err != nil {
					return progress, err
				}
				if mutations != len(target.idempotencyKeys) {
					return progress, fmt.Errorf("%w: cleanup mutation keys changed", ErrConflict)
				}
			}
			err = r.with(false, func(j *Journal) error {
				// The generation comparison covers every legacy/current writer, including
				// a new subscription or lineage link created between the read and write.
				return j.db.Update(func(tx *bbolt.Tx) error {
					if tx.ID() != snapshot.version+1 {
						return ErrConflict
					}
					if err := r.ctx.Err(); err != nil {
						return err
					}
					for _, record := range target.rawRecords {
						var raw []byte
						if bucket := tx.Bucket(record.bucket); bucket != nil {
							raw = bucket.Get(record.key)
						}
						if !bytes.Equal(raw, record.raw) {
							return fmt.Errorf("%w: cleanup metadata changed", ErrConflict)
						}
					}
					nested := tx.Bucket(bEventsByExecution).Bucket([]byte(target.ExecutionID))
					if nested != nil {
						_, latestID := nested.Cursor().Last()
						if string(latestID) != target.lastEventID {
							return fmt.Errorf("%w: cleanup high watermark changed", ErrConflict)
						}
					}
					if final && nested != nil {
						count := 0
						cursor := nested.Cursor()
						for key, _ := cursor.First(); key != nil && count <= n; key, _ = cursor.Next() {
							count++
						}
						if count != n {
							return fmt.Errorf("%w: cleanup event index changed", ErrConflict)
						}
					}
					for _, event := range events[:n] {
						if nested == nil || !bytes.Equal(nested.Get(event.sequence), []byte(event.id)) {
							return fmt.Errorf("%w: cleanup event index changed", ErrConflict)
						}
						if !bytes.Equal(tx.Bucket(bEvents).Get([]byte(event.id)), event.raw) || !bytes.Equal(tx.Bucket(bDedupe).Get([]byte(event.dedupeKey)), event.dedupeRaw) {
							return fmt.Errorf("%w: cleanup event changed", ErrConflict)
						}
					}
					if final {
						// Serialize cache removal only for this short commit, never for the scan.
						outputs, err := diagnostic.OpenOutputs(filepath.Dir(j.path), true)
						if err != nil {
							return err
						}
						defer outputs.Close()
						currentFile, err := outputs.Inspect(target.ExecutionID, target.outputRef)
						if err != nil {
							return err
						}
						if (currentFile == nil) != (target.DiagnosticFile == nil) || (currentFile != nil && *currentFile != *target.DiagnosticFile) {
							return fmt.Errorf("%w: diagnostic file changed; review a new cleanup plan", ErrConflict)
						}
						if target.DiagnosticFile != nil {
							if err := outputs.Remove(*target.DiagnosticFile); err != nil {
								return err
							}
						}
					}
					for _, event := range events[:n] {
						if err := tx.Bucket(bEvents).Delete([]byte(event.id)); err != nil {
							return err
						}
						if err := tx.Bucket(bDedupe).Delete([]byte(event.dedupeKey)); err != nil {
							return err
						}
						nested := tx.Bucket(bEventsByExecution).Bucket([]byte(target.ExecutionID))
						if nested == nil {
							return ErrCorrupt
						}
						if err := nested.Delete(event.sequence); err != nil {
							return err
						}
					}
					if final {
						if tx.Bucket(bEventsByExecution).Bucket([]byte(target.ExecutionID)) != nil {
							if err := tx.Bucket(bEventsByExecution).DeleteBucket([]byte(target.ExecutionID)); err != nil {
								return err
							}
						}
						for _, record := range target.rawRecords {
							if bucket := tx.Bucket(record.bucket); bucket != nil {
								if err := bucket.Delete(record.key); err != nil {
									return err
								}
							}
						}
					}
					return nil
				})
			})
			if err != nil {
				return progress, err
			}
			progress.Batches++
			progress.Events += n
			if final {
				progress.Executions++
			}
			events = events[n:]
			if maxBatches > 0 && progress.Batches >= maxBatches {
				return progress, nil
			}
			if len(events) == 0 {
				break
			}
			// Give blocked journal users a scheduling opportunity after Close.
			select {
			case <-r.ctx.Done():
				return progress, r.ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
	}
	return progress, nil
}

var _ error = (*CleanupInterrupted)(nil)
