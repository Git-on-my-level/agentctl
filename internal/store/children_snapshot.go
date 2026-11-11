package store

import (
	"bytes"
	"context"
	"errors"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

// ChildMembershipErrors proves that each selected parent's retained index is
// represented in the complete hydrated execution snapshot, or validly expired.
// It does not prove collection: inbox separately checks explicit acknowledgements.
// Both snapshot and proof must use this same held read-only handle, excluding
// writers throughout. No subscription, waiter, event or result metadata is read.
// All selected scopes share the existing 100k-entry/64MiB bounds and caller's
// deadline. An error for a scope forbids retiring it from absent execution rows.
func (j *Journal) ChildMembershipErrors(ctx context.Context, parents []ids.ExecutionID, snapshot []model.Execution) map[ids.ExecutionID]error {
	problems := make(map[ids.ExecutionID]error)
	failAll := func(err error) map[ids.ExecutionID]error {
		for _, id := range parents {
			problems[id] = err
		}
		return problems
	}
	if len(parents) == 0 {
		return problems
	}
	if !j.readOnly {
		return failAll(ErrConflict)
	}
	if len(parents) > 100000 {
		return failAll(ErrChildrenBounded)
	}
	byID := make(map[ids.ExecutionID]*model.Execution, len(snapshot))
	for i := range snapshot {
		if err := ctx.Err(); err != nil {
			return failAll(err)
		}
		byID[snapshot[i].ID] = &snapshot[i]
	}
	scanned, totalBytes := 0, 0
	err := j.db.View(func(tx *bbolt.Tx) error {
		indexes, executions := tx.Bucket(bIdempotency), tx.Bucket(bExecutions)
		if indexes == nil || executions == nil {
			return corrupt(errors.New("missing required children index or execution bucket"))
		}
		for _, parent := range parents {
			problems[parent] = func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if scanned > 100000 || totalBytes > 64<<20 {
					return ErrChildrenBounded
				}
				prefix := childrenScopePrefix(ChildrenScope{Parent: &parent})
				// Certification/generation validation remains required. This entire proof
				// is one stable read transaction under the snapshot's shared file lock.
				if _, err := childrenMembershipAt(tx, prefix); err != nil {
					return err
				}
				cursor := indexes.Cursor()
				for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
					if err := ctx.Err(); err != nil {
						return err
					}
					scanned++
					totalBytes += len(raw)
					if scanned > 100000 || totalBytes > 64<<20 || len(raw) > 1<<20 {
						return ErrChildrenBounded
					}
					id, expired, err := decodeChildMembership(tx, prefix, key, raw)
					if err != nil {
						return err
					}
					if expired {
						continue
					}
					child, known := byID[id]
					// A stamp alone, or an absent projection, never proves expiry. Even an
					// acknowledged missing row needs a matching authoritative tombstone.
					rawExecution := executions.Get([]byte(id.String()))
					if !known || rawExecution == nil || len(rawExecution) > 1<<20 {
						return ErrChildrenBounded
					}
					if child.ParentExecutionID == nil || *child.ParentExecutionID != parent {
						return corrupt(errors.New("children parent index mismatch"))
					}
				}
				return nil
			}()
		}
		return nil
	})
	if err != nil {
		return failAll(err)
	}
	return problems
}
