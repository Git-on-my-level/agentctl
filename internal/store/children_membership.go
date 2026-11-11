package store

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"go.etcd.io/bbolt"
)

var (
	bChildrenGenerations = []byte("children_membership_generations")
	keyChildrenWriter    = []byte("children_membership_writer")
	keyChildrenPruned    = []byte("children_membership_pruned")
)

// update certifies every transaction from this binary. A gap in the certified
// transaction sequence means a mixed-version writer may have changed an index
// without updating its scope stamp. Keep that uncertainty across later certified
// writes, so they cannot conceal an intervening legacy mutation.
// All production journal writes must use this entry point, including no-ops.
func (j *Journal) update(fn func(*bbolt.Tx) error) error {
	return j.db.Update(func(tx *bbolt.Tx) error {
		last, legacy, err := childrenWriter(tx)
		if err != nil {
			return err
		}
		id := uint64(tx.ID())
		if last != id-1 {
			legacy = id - 1
		}
		if err := fn(tx); err != nil {
			return err
		}
		raw := make([]byte, 16)
		binary.BigEndian.PutUint64(raw, id)
		binary.BigEndian.PutUint64(raw[8:], legacy)
		return tx.Bucket(bMetadata).Put(keyChildrenWriter, raw)
	})
}

func childrenWriter(tx *bbolt.Tx) (last, legacy uint64, err error) {
	if metadata := tx.Bucket(bMetadata); metadata != nil {
		if raw := metadata.Get(keyChildrenWriter); raw != nil {
			if len(raw) != 16 {
				return 0, 0, fmt.Errorf("%w: children writer certificate", ErrCorrupt)
			}
			last, legacy = binary.BigEndian.Uint64(raw), binary.BigEndian.Uint64(raw[8:])
			if last > uint64(tx.ID()) || legacy > last {
				return 0, 0, fmt.Errorf("%w: children writer sequence", ErrCorrupt)
			}
		}
	}
	return
}

// noteChildrenIndexMutation runs in the same certified write transaction as the
// index Put/Delete, after that operation. Values are transaction IDs, not reusable
// counters. Empty-scope stamps are reclaimed; the pruning fence prevents an ABA
// through an absent stamp, including scopes populated by a legacy binary.
func noteChildrenIndexMutation(tx *bbolt.Tx, key []byte) error {
	if !bytes.HasPrefix(key, []byte(childrenIndexScope)) {
		return nil
	}
	separator := bytes.IndexByte(key, 0)
	if separator < 0 {
		return fmt.Errorf("%w: children index key", ErrCorrupt)
	}
	prefix := key[:separator+1]
	generations, err := tx.CreateBucketIfNotExists(bChildrenGenerations)
	if err != nil {
		return err
	}
	raw := make([]byte, 8)
	binary.BigEndian.PutUint64(raw, uint64(tx.ID()))
	first, _ := tx.Bucket(bIdempotency).Cursor().Seek(prefix)
	if bytes.HasPrefix(first, prefix) {
		return generations.Put(prefix, raw)
	}
	if err := generations.Delete(prefix); err != nil {
		return err
	}
	return tx.Bucket(bMetadata).Put(keyChildrenPruned, raw)
}

type childrenMembership struct{ scope, legacy, pruned uint64 }

// childrenMembershipAt is constant work per page. It reads no unrelated index
// entries and does not require upgrading a legacy journal. Uncertified writes
// fail closed, whereas certified outcome writes and other scopes' admissions do
// not invalidate membership. Pruning an empty scope is conservatively global.
func childrenMembershipAt(tx *bbolt.Tx, prefix []byte) (childrenMembership, error) {
	last, legacy, err := childrenWriter(tx)
	if err != nil {
		return childrenMembership{}, err
	}
	if last != uint64(tx.ID()) {
		legacy = uint64(tx.ID())
	}
	read := func(raw []byte) (uint64, error) {
		if raw == nil {
			return 0, nil
		}
		if len(raw) != 8 || binary.BigEndian.Uint64(raw) > uint64(tx.ID()) {
			return 0, fmt.Errorf("%w: children membership stamp", ErrCorrupt)
		}
		return binary.BigEndian.Uint64(raw), nil
	}
	var stamp childrenMembership
	stamp.legacy = legacy
	if generations := tx.Bucket(bChildrenGenerations); generations != nil {
		stamp.scope, err = read(generations.Get(prefix))
		if err != nil {
			return stamp, err
		}
	}
	stamp.pruned, err = read(tx.Bucket(bMetadata).Get(keyChildrenPruned))
	return stamp, err
}
