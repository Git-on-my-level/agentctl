package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"
)

// Cleanup opens and closes the journal around each page/commit. Closing a
// transaction alone is insufficient: bbolt's process flock lives until Close.
// Open must return a fresh, caller-owned handle and must not create a missing
// journal. Planning always requests a read-only handle.
type Cleanup struct {
	Open func(context.Context, bool) (*Journal, error)
}

const cleanupPageSize = 128
const cleanupPageBytes = 256 << 10
const cleanupEventBatch = 128

type cleanupReader struct {
	ctx  context.Context
	with func(bool, func(*Journal) error) error
	path string
	now  time.Time
}

func (c Cleanup) reader(ctx context.Context) *cleanupReader {
	r := &cleanupReader{ctx: ctx, now: time.Now().UTC()}
	r.with = func(readOnly bool, fn func(*Journal) error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		j, err := c.Open(ctx, readOnly)
		if err != nil {
			return err
		}
		if r.path != "" && r.path != j.path {
			j.Close()
			return ErrConflict
		}
		r.path = j.path
		err = fn(j)
		closeErr := j.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	return r
}

func (j *Journal) cleanupReader(ctx context.Context) *cleanupReader {
	return &cleanupReader{ctx: ctx, path: j.path, now: j.clock().UTC(), with: func(readOnly bool, fn func(*Journal) error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !readOnly && j.readOnly {
			return ErrReadOnly
		}
		return fn(j)
	}}
}

func (r *cleanupReader) view(fn func(*bbolt.Tx) error) error {
	return r.with(true, func(j *Journal) error { return j.db.View(fn) })
}

// scan copies a bounded page out of the transaction before invoking consumers.
// No mmap-backed byte slices escape, and consumers never nest transactions.
func (r *cleanupReader) scan(name, nested []byte, version *int, fn func([]byte, []byte) error) error {
	var after []byte
	for {
		type row struct{ key, value []byte }
		page := []row{}
		err := r.view(func(tx *bbolt.Tx) error {
			if version != nil {
				if *version == 0 {
					*version = tx.ID()
				} else if *version != tx.ID() {
					return ErrConflict
				}
			}
			bucket := tx.Bucket(name)
			if bucket == nil {
				return nil
			}
			if nested != nil {
				bucket = bucket.Bucket(nested)
				if bucket == nil {
					return nil
				}
			}
			cursor := bucket.Cursor()
			key, value := cursor.First()
			if after != nil {
				key, value = cursor.Seek(after)
				if bytes.Equal(key, after) {
					key, value = cursor.Next()
				}
			}
			size := 0
			for ; key != nil && len(page) < cleanupPageSize && size < cleanupPageBytes; key, value = cursor.Next() {
				if err := r.ctx.Err(); err != nil {
					return err
				}
				if value == nil {
					return fmt.Errorf("%w: unexpected cleanup bucket", ErrCorrupt)
				}
				page = append(page, row{bytes.Clone(key), bytes.Clone(value)})
				size += len(key) + len(value)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			if err := fn(row.key, row.value); err != nil {
				return err
			}
		}
		after = page[len(page)-1].key
	}
}

type cleanupBucket map[string][]byte

func (b cleanupBucket) Get(key []byte) []byte { return b[string(key)] }
func (b cleanupBucket) ForEach(fn func([]byte, []byte) error) error {
	keys := make([]string, 0, len(b))
	for key := range b {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := fn([]byte(key), b[key]); err != nil {
			return err
		}
	}
	return nil
}

type cleanupSnapshot struct {
	ctx     context.Context
	buckets map[string]cleanupBucket
	version int
}

func (s *cleanupSnapshot) Bucket(name []byte) cleanupBucket { return s.buckets[string(name)] }
func (s *cleanupSnapshot) acknowledgementEpoch() (time.Time, error) {
	raw := s.Bucket(bMetadata).Get(keyAckEpoch)
	if len(raw) == 0 {
		return time.Time{}, nil
	}
	value, err := time.Parse(time.RFC3339Nano, string(raw))
	if err != nil {
		return time.Time{}, corrupt(err)
	}
	return value, nil
}

// A stable generation covers only protection metadata, never event history.
// If writers interleave, defer instead of deleting from a torn reference graph.
// Old binaries participate automatically because this uses bbolt's tx ID.
func (r *cleanupReader) snapshot(stable bool) (*cleanupSnapshot, error) {
	if !stable {
		return r.snapshotOnce(false)
	}
	for attempt := 0; attempt < 3; attempt++ {
		snapshot, err := r.snapshotOnce(true)
		if err != ErrConflict {
			return snapshot, err
		}
	}
	return nil, fmt.Errorf("%w: cleanup protection metadata changed during paged scan; retry later", ErrConflict)
}

func (r *cleanupReader) snapshotOnce(stable bool) (*cleanupSnapshot, error) {
	s := &cleanupSnapshot{ctx: r.ctx, buckets: map[string]cleanupBucket{}}
	for _, name := range [][]byte{bMetadata, bExecutions, bSubscriptions, bOutbox, bOutboxDead, bOutboxReceipts, bAcknowledgements, bIdempotency} {
		bucket := cleanupBucket{}
		s.buckets[string(name)] = bucket
		var version *int
		if stable {
			version = &s.version
		}
		if err := r.scan(name, nil, version, func(key, raw []byte) error { bucket[string(key)] = raw; return nil }); err != nil {
			return nil, err
		}
	}
	// Only subscription cursors and deliveries lacking execution IDs need event
	// payloads for protection. Never load the full events bucket into memory.
	eventIDs := map[string]bool{}
	if err := s.Bucket(bSubscriptions).ForEach(func(_, raw []byte) error {
		var record SubscriptionRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return corrupt(err)
		}
		if record.Subscription.Cursor.EventID != "" {
			eventIDs[record.Subscription.Cursor.EventID] = true
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, name := range [][]byte{bOutbox, bOutboxDead} {
		if err := s.Bucket(name).ForEach(func(_, raw []byte) error {
			item, err := decodeDelivery(raw)
			if err != nil {
				return err
			}
			if item.ExecutionID == "" {
				eventIDs[item.EventID] = true
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	s.buckets[string(bEvents)] = cleanupBucket{}
	ids := make([]string, 0, len(eventIDs))
	for id := range eventIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for len(ids) > 0 {
		n := min(len(ids), cleanupPageSize)
		if err := r.view(func(tx *bbolt.Tx) error {
			if stable && tx.ID() != s.version {
				return ErrConflict
			}
			for _, id := range ids[:n] {
				if raw := tx.Bucket(bEvents).Get([]byte(id)); raw != nil {
					s.Bucket(bEvents)[id] = bytes.Clone(raw)
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		ids = ids[n:]
	}
	return s, nil
}
