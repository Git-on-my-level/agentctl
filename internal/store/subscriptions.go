package store

// This file contains the durable subscription projection.  Subscription
// policy remains in internal/subscription; this package only supplies the
// journal transaction, idempotency, and revision semantics.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

var (
	// Keep the journal-wide errors discoverable through errors.Is while the
	// descriptive names make call sites self-documenting.
	ErrSubscriptionNotFound = ErrNotFound
	ErrSubscriptionConflict = ErrConflict
)

var (
	bSubscriptions = []byte("subscriptions_v1")
)

// SubscriptionRecord is the durable revision envelope.  Revision starts at
// one and is incremented by every successful UpdateSubscription call.
type SubscriptionRecord struct {
	Subscription subscription.Subscription `json:"subscription"`
	Revision     uint64                    `json:"revision"`
	CreatedAt    time.Time                 `json:"created_at"`
	UpdatedAt    time.Time                 `json:"updated_at"`
}

// PutSubscription stores a subscription exactly once.  Repeating the same
// canonical value is idempotent and returns reused=true.  Reusing an ID for a
// different value is a conflict rather than an in-place overwrite.
func (j *Journal) PutSubscription(ctx context.Context, value subscription.Subscription) (subscription.Subscription, bool, error) {
	if j.readOnly {
		return subscription.Subscription{}, false, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return subscription.Subscription{}, false, err
	}
	var result subscription.Subscription
	var reused bool
	err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := ensureJournalBucket(tx, bSubscriptions)
		if err != nil {
			return err
		}
		now := j.clock().UTC()
		value, err = normalizeStoredSubscription(value, now, j.generator, bucket, tx.Bucket(bMetadata))
		if err != nil {
			return err
		}
		key := []byte(value.ID)
		if raw := bucket.Get(key); raw != nil {
			var existing SubscriptionRecord
			if err := json.Unmarshal(raw, &existing); err != nil {
				return corrupt(err)
			}
			if err := existing.Subscription.Validate(now); err != nil {
				return corrupt(err)
			}
			if subscriptionEqual(existing.Subscription, value) {
				result = existing.Subscription
				reused = true
				return nil
			}
			return fmt.Errorf("%w: ID %q already exists", ErrSubscriptionConflict, value.ID)
		}
		record := SubscriptionRecord{Subscription: value, Revision: 1, CreatedAt: now, UpdatedAt: now}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := bucket.Put(key, raw); err != nil {
			return err
		}
		result = value
		return nil
	})
	return result, reused, err
}

// CreateSubscription is a descriptive alias for PutSubscription.
func (j *Journal) CreateSubscription(ctx context.Context, value subscription.Subscription) (subscription.Subscription, bool, error) {
	return j.PutSubscription(ctx, value)
}

func (j *Journal) GetSubscription(ctx context.Context, id string) (subscription.Subscription, error) {
	record, err := j.GetSubscriptionRecord(ctx, id)
	return record.Subscription, err
}

// GetSubscriptionRecord exposes the CAS revision without making Revision a
// part of the storage-neutral subscription contract.
func (j *Journal) GetSubscriptionRecord(ctx context.Context, id string) (SubscriptionRecord, error) {
	if err := ctx.Err(); err != nil {
		return SubscriptionRecord{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return SubscriptionRecord{}, ErrSubscriptionNotFound
	}
	if _, err := ids.ParseSubscriptionID(id); err != nil {
		return SubscriptionRecord{}, fmt.Errorf("%w: invalid subscription ID: %v", ErrSubscriptionConflict, err)
	}
	var result SubscriptionRecord
	err := j.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bSubscriptions)
		if bucket == nil {
			return ErrSubscriptionNotFound
		}
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return ErrSubscriptionNotFound
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return corrupt(err)
		}
		if result.Revision == 0 || result.Subscription.ID != id {
			return fmt.Errorf("%w: malformed subscription record", ErrCorrupt)
		}
		if err := validateStoredSubscriptionIdentity(result.Subscription); err != nil {
			return err
		}
		if err := result.Subscription.Validate(time.Time{}); err != nil {
			return corrupt(err)
		}
		return nil
	})
	if err == nil && result.Subscription.State == subscription.StateActive && !result.Subscription.ExpiresAt.IsZero() && !j.clock().UTC().Before(result.Subscription.ExpiresAt) {
		result.Subscription.State = subscription.StateExpired
	}
	return result, err
}

// ListSubscriptions returns all records in stable ID order.  Expired records
// are retained so callers can inspect or explicitly cancel them.
func (j *Journal) ListSubscriptions(ctx context.Context) ([]subscription.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := make([]subscription.Subscription, 0)
	err := j.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bSubscriptions)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, raw []byte) error {
			var record SubscriptionRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return corrupt(err)
			}
			if err := record.Subscription.Validate(time.Time{}); err != nil {
				return corrupt(err)
			}
			if err := validateStoredSubscriptionIdentity(record.Subscription); err != nil {
				return err
			}
			value := record.Subscription
			if value.State == subscription.StateActive && !value.ExpiresAt.IsZero() && !j.clock().UTC().Before(value.ExpiresAt) {
				value.State = subscription.StateExpired
			}
			values = append(values, value)
			return nil
		})
	})
	sort.Slice(values, func(i, k int) bool { return values[i].ID < values[k].ID })
	return values, err
}

// UpdateSubscription applies a compare-and-swap update.  expectedRevision
// must be the value returned by GetSubscriptionRecord.
func (j *Journal) UpdateSubscription(ctx context.Context, value subscription.Subscription, expectedRevision uint64) (subscription.Subscription, error) {
	if j.readOnly {
		return subscription.Subscription{}, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return subscription.Subscription{}, err
	}
	if expectedRevision == 0 {
		return subscription.Subscription{}, fmt.Errorf("%w: expected revision must be positive", ErrSubscriptionConflict)
	}
	var result subscription.Subscription
	err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := ensureJournalBucket(tx, bSubscriptions)
		if err != nil {
			return err
		}
		raw := bucket.Get([]byte(value.ID))
		if raw == nil {
			return ErrSubscriptionNotFound
		}
		var previous SubscriptionRecord
		if err := json.Unmarshal(raw, &previous); err != nil {
			return corrupt(err)
		}
		if previous.Revision != expectedRevision {
			return fmt.Errorf("%w: expected revision %d, current %d", ErrSubscriptionConflict, expectedRevision, previous.Revision)
		}
		value, err = normalizeStoredSubscription(value, j.clock().UTC(), j.generator, bucket, tx.Bucket(bMetadata))
		if err != nil {
			return err
		}
		if value.ID != previous.Subscription.ID {
			return fmt.Errorf("%w: subscription ID is immutable", ErrSubscriptionConflict)
		}
		now := j.clock().UTC()
		next := SubscriptionRecord{Subscription: value, Revision: expectedRevision + 1, CreatedAt: previous.CreatedAt, UpdatedAt: now}
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(value.ID), encoded); err != nil {
			return err
		}
		result = value
		return nil
	})
	return result, err
}

// UpdateSubscriptionCAS is an explicit spelling useful to callers with
// several journal resources in scope.
func (j *Journal) UpdateSubscriptionCAS(ctx context.Context, value subscription.Subscription, expectedRevision uint64) (subscription.Subscription, error) {
	return j.UpdateSubscription(ctx, value, expectedRevision)
}

func ensureJournalBucket(tx *bbolt.Tx, name []byte) (*bbolt.Bucket, error) {
	return tx.CreateBucketIfNotExists(name)
}

func validateStoredSubscriptionIdentity(value subscription.Subscription) error {
	if _, err := ids.ParseSubscriptionID(value.ID); err != nil {
		return fmt.Errorf("%w: invalid subscription ID: %v", ErrCorrupt, err)
	}
	if _, err := ids.ParseHostID(value.Origin); err != nil {
		return fmt.Errorf("%w: invalid origin host: %v", ErrCorrupt, err)
	}
	for _, executionID := range value.Filter.ExecutionIDs {
		if _, err := ids.ParseExecutionID(strings.TrimSpace(executionID)); err != nil {
			return fmt.Errorf("%w: invalid filter execution ID: %v", ErrCorrupt, err)
		}
	}
	for _, id := range []struct {
		name  string
		value string
		parse func(string) (ids.ID, error)
	}{
		{"coordinator execution ID", value.CoordinatorExecutionID, func(v string) (ids.ID, error) { return ids.ParseAs(ids.TypeExecution, v) }},
		{"replacement ID", value.Replaces, func(v string) (ids.ID, error) { return ids.ParseAs(ids.TypeSubscription, v) }},
		{"replaced-by ID", value.ReplacedBy, func(v string) (ids.ID, error) { return ids.ParseAs(ids.TypeSubscription, v) }},
	} {
		if id.value != "" {
			if _, err := id.parse(strings.TrimSpace(id.value)); err != nil {
				return fmt.Errorf("%w: invalid %s: %v", ErrCorrupt, id.name, err)
			}
		}
	}
	if value.Cursor.ID != "" {
		if _, err := ids.ParseCursorID(value.Cursor.ID); err != nil {
			return fmt.Errorf("%w: invalid cursor ID: %v", ErrCorrupt, err)
		}
	}
	if value.Cursor.Origin != "" {
		if _, err := ids.ParseHostID(value.Cursor.Origin); err != nil {
			return fmt.Errorf("%w: invalid cursor origin: %v", ErrCorrupt, err)
		}
	}
	if value.Cursor.EventID != "" {
		if _, err := ids.ParseEventID(value.Cursor.EventID); err != nil {
			return fmt.Errorf("%w: invalid cursor event ID: %v", ErrCorrupt, err)
		}
	}
	return nil
}

func normalizeStoredSubscription(value subscription.Subscription, now time.Time, generator ids.Generator, bucket *bbolt.Bucket, metadata *bbolt.Bucket) (subscription.Subscription, error) {
	if value.ID == "" {
		if generator == nil {
			return subscription.Subscription{}, errors.New("subscription ID is required")
		}
		for range 16 {
			id, err := generator.New(ids.TypeSubscription)
			if err != nil {
				return subscription.Subscription{}, err
			}
			if bucket.Get([]byte(id.String())) == nil {
				value.ID = id.String()
				break
			}
		}
		if value.ID == "" {
			return subscription.Subscription{}, fmt.Errorf("%w: exhausted subscription ID collisions", ErrSubscriptionConflict)
		}
	}
	if value.Origin == "" {
		// The origin is a journal property. Resolve it transactionally so a
		// caller does not need to race a separate HostID read.
		if metadata == nil || metadata.Get(keyHost) == nil {
			return subscription.Subscription{}, fmt.Errorf("%w: missing journal origin host", ErrCorrupt)
		}
		value.Origin = string(metadata.Get(keyHost))
	}
	if _, err := ids.ParseSubscriptionID(value.ID); err != nil {
		return subscription.Subscription{}, fmt.Errorf("%w: invalid subscription ID: %v", ErrSubscriptionConflict, err)
	}
	origin, err := ids.ParseHostID(value.Origin)
	if err != nil {
		return subscription.Subscription{}, fmt.Errorf("%w: invalid origin host: %v", ErrSubscriptionConflict, err)
	}
	if metadata != nil && metadata.Get(keyHost) != nil && string(origin) != string(metadata.Get(keyHost)) {
		return subscription.Subscription{}, fmt.Errorf("%w: origin host does not match journal", ErrSubscriptionConflict)
	}
	for _, executionID := range value.Filter.ExecutionIDs {
		if _, err := ids.ParseExecutionID(strings.TrimSpace(executionID)); err != nil {
			return subscription.Subscription{}, fmt.Errorf("%w: invalid filter execution ID: %v", ErrSubscriptionConflict, err)
		}
	}
	for _, id := range []struct {
		name  string
		value string
		parse func(string) (ids.ID, error)
	}{
		{"coordinator execution ID", value.CoordinatorExecutionID, func(v string) (ids.ID, error) { return ids.ParseAs(ids.TypeExecution, v) }},
		{"replacement ID", value.Replaces, func(v string) (ids.ID, error) { return ids.ParseAs(ids.TypeSubscription, v) }},
		{"replaced-by ID", value.ReplacedBy, func(v string) (ids.ID, error) { return ids.ParseAs(ids.TypeSubscription, v) }},
	} {
		if id.value != "" {
			if _, err := id.parse(strings.TrimSpace(id.value)); err != nil {
				return subscription.Subscription{}, fmt.Errorf("%w: invalid %s: %v", ErrSubscriptionConflict, id.name, err)
			}
		}
	}
	if value.Cursor.ID != "" {
		if _, err := ids.ParseCursorID(value.Cursor.ID); err != nil {
			return subscription.Subscription{}, fmt.Errorf("%w: invalid cursor ID: %v", ErrSubscriptionConflict, err)
		}
	}
	if value.Cursor.Origin != "" {
		if _, err := ids.ParseHostID(value.Cursor.Origin); err != nil {
			return subscription.Subscription{}, fmt.Errorf("%w: invalid cursor origin: %v", ErrSubscriptionConflict, err)
		}
	}
	if value.Cursor.EventID != "" {
		if _, err := ids.ParseEventID(value.Cursor.EventID); err != nil {
			return subscription.Subscription{}, fmt.Errorf("%w: invalid cursor event ID: %v", ErrSubscriptionConflict, err)
		}
	}
	filter, err := value.Filter.Normalize()
	if err != nil {
		return subscription.Subscription{}, err
	}
	value.Filter = filter
	value.Destination = cloneDestination(value.Destination)
	if value.Destination.Kind == callback.DestinationCommand {
		plan, err := callback.PlanCommand(value.Destination)
		if err != nil {
			return subscription.Subscription{}, fmt.Errorf("%w: command executable identity: %v", ErrSubscriptionConflict, err)
		}
		identity := plan.Identity()
		value.Destination.CommandIdentity = &identity
	}
	if value.ResolvedScope != nil {
		value.ResolvedScope = cloneStringMap(value.ResolvedScope)
	}
	if value.State == "" {
		value.State = subscription.StateActive
	}
	if err := value.Validate(now); err != nil {
		return subscription.Subscription{}, err
	}
	return value, nil
}

func cloneDestination(value callback.Destination) callback.Destination {
	value.Args = append([]string(nil), value.Args...)
	value.AllowedHosts = append([]string(nil), value.AllowedHosts...)
	if value.CommandIdentity != nil {
		identity := *value.CommandIdentity
		value.CommandIdentity = &identity
	}
	return value
}

func cloneStringMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func subscriptionEqual(a, b subscription.Subscription) bool {
	// JSON equality gives maps deterministic semantics and avoids treating a
	// caller's nil/empty optional slices as an accidental mutation.
	aa, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return reflect.DeepEqual(a, b)
	}
	return string(aa) == string(bb)
}
