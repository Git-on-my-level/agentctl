package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"go.etcd.io/bbolt"
)

const RetentionPreviewWindow = 7 * 24 * time.Hour

// RetentionEnforceAt is persisted on the first writable open of this version.
// New journals enforce immediately; upgraded journals preview for seven days.
// Read-only opens never initialize or advance the window. Missing metadata is
// unknown (plan-only), never permission to delete.
func (j *Journal) RetentionEnforceAt(ctx context.Context) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	var at time.Time
	err := j.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bMetadata).Get(keyRetentionEnforceAt)
		if raw == nil {
			return nil
		}
		var err error
		at, err = time.Parse(time.RFC3339Nano, string(raw))
		if err != nil {
			return fmt.Errorf("%w: retention enforcement time: %v", ErrCorrupt, err)
		}
		return nil
	})
	return at, err
}

// ProtectANXRun must commit before spooling/sending a run link. It is monotonic
// and idempotent, independent of collection, spool expiry and remote reachability.
func (j *Journal) ProtectANXRun(ctx context.Context, id ids.ExecutionID) error {
	if j.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(bExecutions).Get([]byte(id.String())) == nil {
			return ErrNotFound
		}
		b, err := tx.CreateBucketIfNotExists(bANXRunLinks)
		if err != nil {
			return err
		}
		if b.Get([]byte(id.String())) != nil {
			return nil
		}
		return b.Put([]byte(id.String()), []byte("registered"))
	})
}

// ANXRunProtected allows registration retries to reuse their durable stamp
// through the read handle already held for the execution, without another
// writable open or a no-op commit on the journal's contention path.
func (j *Journal) ANXRunProtected(ctx context.Context, id ids.ExecutionID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var protected bool
	err := j.db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket(bANXRunLinks); b != nil {
			protected = b.Get([]byte(id.String())) != nil
		}
		return nil
	})
	return protected, err
}
