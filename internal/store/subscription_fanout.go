package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

// enqueueMatchingSubscriptionsTx is called by AppendEvent while it still
// owns the event transaction. It fans out one bounded callback envelope per
// matching active subscription; enqueueDeliveryTx makes each pair durable and
// deduplicated in that same transaction.
//
// canonicalPayload is intentionally not used as the callback body. It is the
// event's semantic projection and may contain adapter material that must not
// cross a callback boundary. The normalized model.Event is the only event
// document serialized into an envelope.
func enqueueMatchingSubscriptionsTx(tx *bbolt.Tx, event *model.Event, canonicalPayload []byte, now time.Time, generator ids.Generator) error {
	if event == nil {
		return fmt.Errorf("%w: nil event for subscription fan-out", ErrCorrupt)
	}
	_ = canonicalPayload
	if err := event.Validate(); err != nil {
		return fmt.Errorf("event fan-out validation: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	subs := tx.Bucket(bSubscriptions)
	if subs == nil {
		return nil
	}
	if _, err := ensureJournalBucket(tx, bOutbox); err != nil {
		return err
	}
	if _, err := ensureJournalBucket(tx, bOutboxDedupe); err != nil {
		return err
	}
	err := subs.ForEach(func(key, raw []byte) error {
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
		matchesScope := matchesEventScope(record.Subscription, event, now)
		if !matchesScope || !matchesEventKind(record.Subscription, event) {
			return nil
		}
		expires := record.Subscription.ExpiresAt
		if expires.IsZero() {
			expires = now.Add(defaultDeliveryTTL)
		}
		dedupe := deliveryDedupeKey(record.Subscription.ID, event.DedupeKey)
		if existing := tx.Bucket(bOutboxDedupe); existing != nil && existing.Get([]byte(dedupe)) != nil {
			// Existing delivery is authoritative. AppendEvent retries are
			// expected to converge without allocating a new delivery ID.
			return nil
		}
		if _, err := ensureJournalBucket(tx, bOutbox); err != nil {
			return err
		}
		deliveryID, err := newDeliveryID(tx, generator)
		if err != nil {
			return err
		}
		nonceSum := sha256.Sum256([]byte("agentctl-callback-nonce-v1\x00" + deliveryID))
		nonce := hex.EncodeToString(nonceSum[:])
		eventBody, err := callback.NewEnvelope(deliveryID, record.Subscription.ID, event.ID.String(), event.DedupeKey, nonce, 1, now, expires, event)
		if err != nil {
			return err
		}
		payload, err := eventBody.CanonicalBody()
		if err != nil {
			return err
		}
		_, _, err = enqueueDeliveryTx(tx, DeliveryRequest{DeliveryID: deliveryID, ExecutionID: event.ExecutionID.String(), SubscriptionID: record.Subscription.ID, EventID: event.ID.String(), EventDedupeKey: event.DedupeKey, Payload: payload, CreatedAt: now, NextAttemptAt: now, ExpiresAt: expires}, now, generator)
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

func matchesEvent(value subscription.Subscription, event *model.Event, now time.Time) bool {
	return matchesEventScope(value, event, now) && matchesEventKind(value, event)
}

func matchesEventScope(value subscription.Subscription, event *model.Event, now time.Time) bool {
	if value.State != subscription.StateActive {
		return false
	}
	if !value.ExpiresAt.IsZero() && !now.Before(value.ExpiresAt) {
		return false
	}
	filter := value.Filter
	if len(filter.ExecutionIDs) > 0 {
		matched := false
		for _, executionID := range filter.ExecutionIDs {
			if strings.TrimSpace(executionID) == event.ExecutionID.String() {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if filter.Authority != "" && filter.Authority != string(event.Authority) {
		return false
	}
	// Query and scope evaluation requires a query planner/event scope
	// contract. Never treat them as a broad wildcard in the storage layer.
	if strings.TrimSpace(filter.Query) != "" || len(filter.Scope) > 0 {
		return false
	}
	return true
}

func matchesEventKind(value subscription.Subscription, event *model.Event) bool {
	if len(value.Filter.Kinds) > 0 {
		matched := false
		for _, kind := range value.Filter.Kinds {
			if strings.TrimSpace(kind) == string(event.Kind) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
