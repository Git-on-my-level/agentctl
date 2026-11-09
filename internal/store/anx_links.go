package store

import (
	"context"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"go.etcd.io/bbolt"
)

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
