package store

import (
	"context"
	"encoding/json"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

// RecordStatusContact remembers a successful identical status_api poll without
// rewriting the execution envelope. Inbox uses the later of this timestamp and
// observation.observed_at so a live task is not reported stale between envelope
// refreshes.
func (j *Journal) RecordStatusContact(ctx context.Context, id ids.ExecutionID, at time.Time) error {
	if j.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if at.IsZero() {
		return nil
	}
	return j.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(bExecutions).Get([]byte(id.String())) == nil {
			return ErrNotFound
		}
		bucket, err := ensureJournalBucket(tx, bStatusContacts)
		if err != nil {
			return err
		}
		key := []byte(id.String())
		if existing := bucket.Get(key); existing != nil {
			var prev time.Time
			if err := json.Unmarshal(existing, &prev); err != nil {
				return corrupt(err)
			}
			if !at.After(prev) {
				return nil
			}
		}
		encoded, err := json.Marshal(at.UTC())
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
}

// StatusContactIndex returns last successful identical-poll times. A journal
// that predates the bucket contributes an empty map.
func (j *Journal) StatusContactIndex(ctx context.Context) (map[ids.ExecutionID]time.Time, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index := map[ids.ExecutionID]time.Time{}
	err := j.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bStatusContacts)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(key, raw []byte) error {
			id, err := ids.ParseExecutionID(string(key))
			if err != nil {
				return corrupt(err)
			}
			var at time.Time
			if err := json.Unmarshal(raw, &at); err != nil {
				return corrupt(err)
			}
			index[id] = at
			return nil
		})
	})
	return index, err
}
