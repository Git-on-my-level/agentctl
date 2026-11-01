package store

import (
	"bytes"
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
	// Label-prefix subscriptions need the emitting execution's immutable
	// labels. Read them once, lazily, inside this same transaction.
	var labels []string
	labelsLoaded := false
	executionLabels := func() []string {
		if !labelsLoaded {
			labelsLoaded = true
			labels = executionLabelsTx(tx, event.ExecutionID)
		}
		return labels
	}
	err := subs.ForEach(func(key, raw []byte) error {
		var record SubscriptionRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return corrupt(err)
		}
		if !fanoutUnderstands(raw, record.Subscription) {
			return nil
		}
		if err := validateStoredSubscriptionIdentity(record.Subscription); err != nil {
			return err
		}
		matchesScope := matchesEventScope(record.Subscription, event, now, executionLabels)
		if !matchesScope || !matchesEventKind(record.Subscription, event) {
			return nil
		}
		return enqueueSubscriptionEventTx(tx, record.Subscription, event, now, generator)
	})
	if err != nil {
		return err
	}
	return nil
}

// fanoutUnderstands reports whether this release can evaluate a stored
// subscription. A journal is shared with other releases: owners started before
// an upgrade keep appending events after a newer binary writes a subscription
// they cannot read. Failing the append for that would kill an unrelated
// execution (2026-10-25 m5 incident), so such a record gets no delivery from
// this release instead. Dropping an unknown filter field would widen the match,
// so an unknown field also skips the record. Unparsable JSON stays corruption.
func fanoutUnderstands(raw []byte, value subscription.Subscription) bool {
	if value.State != subscription.StateActive || value.Validate(time.Time{}) != nil {
		return false
	}
	var stored struct {
		Subscription struct {
			Filter json.RawMessage `json:"filter"`
		} `json:"subscription"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(stored.Subscription.Filter))
	decoder.DisallowUnknownFields()
	var filter subscription.EventFilter
	return decoder.Decode(&filter) == nil
}

// enqueueSubscriptionEventTx enqueues one deduplicated delivery of event for
// one already-matched subscription.
func enqueueSubscriptionEventTx(tx *bbolt.Tx, value subscription.Subscription, event *model.Event, now time.Time, generator ids.Generator) error {
	expires := value.ExpiresAt
	if expires.IsZero() {
		expires = now.Add(defaultDeliveryTTL)
	}
	dedupe := deliveryDedupeKey(value.ID, event.DedupeKey)
	if existing := tx.Bucket(bOutboxDedupe); existing != nil && existing.Get([]byte(dedupe)) != nil {
		// Existing delivery is authoritative. AppendEvent retries are
		// expected to converge without allocating a new delivery ID.
		return nil
	}
	if _, err := ensureJournalBucket(tx, bOutbox); err != nil {
		return err
	}
	if _, err := ensureJournalBucket(tx, bOutboxDedupe); err != nil {
		return err
	}
	deliveryID, err := newDeliveryID(tx, generator)
	if err != nil {
		return err
	}
	nonceSum := sha256.Sum256([]byte("agentctl-callback-nonce-v1\x00" + deliveryID))
	nonce := hex.EncodeToString(nonceSum[:])
	eventBody, err := callback.NewEnvelope(deliveryID, value.ID, event.ID.String(), event.DedupeKey, nonce, 1, now, expires, event)
	if err != nil {
		return err
	}
	payload, err := eventBody.CanonicalBody()
	if err != nil {
		return err
	}
	_, _, err = enqueueDeliveryTx(tx, DeliveryRequest{DeliveryID: deliveryID, ExecutionID: event.ExecutionID.String(), SubscriptionID: value.ID, EventID: event.ID.String(), EventDedupeKey: event.DedupeKey, Payload: payload, CreatedAt: now, NextAttemptAt: now, ExpiresAt: expires}, now, generator)
	return err
}

// enqueueTerminalBackfillTx makes an exact-execution subscription level-
// triggered for terminal state: when a named execution is already terminal at
// creation, its recorded terminal event is enqueued in the creating
// transaction. Without it a subscription made moments after the execution
// ended waits forever, because fan-out only runs when an event is appended.
// Label-prefix and authority scopes stay forward-only (no backfill).
func enqueueTerminalBackfillTx(tx *bbolt.Tx, value subscription.Subscription, now time.Time, generator ids.Generator) error {
	terminals := tx.Bucket(bTerminal)
	events := tx.Bucket(bEvents)
	if terminals == nil || events == nil || len(value.Filter.ExecutionIDs) == 0 {
		return nil
	}
	for _, raw := range value.Filter.ExecutionIDs {
		eventID := terminals.Get([]byte(strings.TrimSpace(raw)))
		if eventID == nil {
			continue
		}
		encoded := events.Get(eventID)
		if encoded == nil {
			return fmt.Errorf("%w: terminal event %s for execution %s is missing", ErrCorrupt, eventID, raw)
		}
		var stored storedEvent
		if err := json.Unmarshal(encoded, &stored); err != nil {
			return corrupt(err)
		}
		event := stored.Event
		if !matchesEvent(value, &event, now, func() []string { return executionLabelsTx(tx, event.ExecutionID) }) {
			continue
		}
		if err := enqueueSubscriptionEventTx(tx, value, &event, now, generator); err != nil {
			return fmt.Errorf("backfill terminal event for execution %s: %w", raw, err)
		}
	}
	return nil
}

func matchesEvent(value subscription.Subscription, event *model.Event, now time.Time, labels func() []string) bool {
	return matchesEventScope(value, event, now, labels) && matchesEventKind(value, event)
}

// executionLabelsTx returns nil when the execution record is unavailable; a
// label-prefix subscription then fails closed for that event.
func executionLabelsTx(tx *bbolt.Tx, id ids.ExecutionID) []string {
	bucket := tx.Bucket(bExecutions)
	if bucket == nil {
		return nil
	}
	raw := bucket.Get([]byte(id.String()))
	if raw == nil {
		return nil
	}
	var value struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value.Labels
}

func matchesEventScope(value subscription.Subscription, event *model.Event, now time.Time, labels func() []string) bool {
	if value.State != subscription.StateActive {
		return false
	}
	if !value.ExpiresAt.IsZero() && !now.Before(value.ExpiresAt) {
		return false
	}
	filter := value.Filter
	// Normalize aliases at match time as well as creation time so legacy
	// subscriptions written with the user-facing `direct` authority continue
	// to match native events after upgrade.
	normalized, err := filter.Normalize()
	if err != nil {
		return false
	}
	filter = normalized
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
	if filter.LabelPrefix != "" {
		if labels == nil {
			return false
		}
		matched := false
		for _, label := range labels() {
			if strings.HasPrefix(label, filter.LabelPrefix) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	// Query and scope evaluation requires a query planner/event scope
	// contract. Never treat them as a broad wildcard in the storage layer.
	if strings.TrimSpace(filter.Query) != "" || len(filter.Scope) > 0 {
		return false
	}
	return true
}

func matchesEventKind(value subscription.Subscription, event *model.Event) bool {
	if event == nil {
		return false
	}
	matched := false
	for _, kind := range value.Filter.Kinds {
		if strings.TrimSpace(kind) == string(event.Kind) {
			matched = true
			break
		}
	}
	// acknowledged is a collection stamp. It stays in the journal event log
	// and is delivered only when a subscription names that kind. An empty
	// kind list, including subscribe --kind all, matches every other kind
	// and does not match acknowledged.
	if event.Kind == model.EventAcknowledged {
		return matched
	}
	if len(value.Filter.Kinds) > 0 && !matched {
		return false
	}
	return true
}
