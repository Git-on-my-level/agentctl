package store

// The callback outbox is deliberately kept in the same bbolt database as the
// journal.  Enqueuing a delivery and recording its dedupe key therefore share
// one transaction, so a crash cannot produce two deliveries for one
// subscription/event pair.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

var (
	// Keep the journal-wide errors discoverable through errors.Is while the
	// descriptive names make call sites self-documenting.
	ErrDeliveryNotFound = ErrNotFound
	ErrDeliveryConflict = ErrConflict
)

var deliveryDedupePattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const (
	// Callback envelopes are bounded by callback's webhook profile. Keep the
	// same limit for every destination, including local transports.
	MaxOutboxPayload   = 1 << 20
	defaultDeliveryTTL = 24 * time.Hour
	maxReceiptText     = 256
)

var (
	bOutbox         = []byte("callback_outbox_v1")
	bOutboxDedupe   = []byte("callback_outbox_dedupe_v1")
	bOutboxReceipts = []byte("callback_outbox_receipts_v1")
	bOutboxDead     = []byte("callback_outbox_dead_v1")
)

// DeliveryRequest is the atomic enqueue projection. Payload must be one
// bounded JSON callback document; it is never included in errors or logs.
type DeliveryRequest struct {
	DeliveryID     string
	ExecutionID    string
	SubscriptionID string
	EventID        string
	EventDedupeKey string
	Payload        []byte
	CreatedAt      time.Time
	NextAttemptAt  time.Time
	ExpiresAt      time.Time
	TTL            time.Duration
}

// DeliveryRecord is the durable callback handle. Payload is intentionally
// omitted from JSON projections so status/errors cannot accidentally expose
// an event body; it remains available to a trusted delivery adapter.
type DeliveryRecord struct {
	ID             string `json:"id"`
	ExecutionID    string `json:"execution_id,omitempty"`
	SubscriptionID string `json:"subscription_id"`
	EventID        string `json:"event_id"`
	EventDedupeKey string `json:"event_dedupe_key"`
	Attempts       int    `json:"attempts"`
	// AttemptInFlight is persisted before transport I/O. A crash/restart can
	// resend the exact cached envelope (nonce, sent_at, expiry) and let the
	// receiver return its original acknowledgement; a scheduled retry clears
	// this bit and creates a fresh attempt.
	AttemptInFlight bool             `json:"attempt_in_flight,omitempty"`
	NextAttemptAt   time.Time        `json:"next_attempt_at,omitempty"`
	ExpiresAt       time.Time        `json:"expires_at,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	State           string           `json:"state"`
	LastError       string           `json:"last_error,omitempty"`
	Payload         []byte           `json:"-"`
	Receipt         *DeliveryReceipt `json:"receipt,omitempty"`
	PausedAt        time.Time        `json:"paused_at,omitempty"`
	DeadLetterAt    time.Time        `json:"dead_letter_at,omitempty"`
	DeadLetterWhy   string           `json:"dead_letter_reason,omitempty"`
}

// OutboxDelivery is an alias for adapters that prefer the outbox terminology.
type OutboxDelivery = DeliveryRecord
type OutboxRequest = DeliveryRequest
type Receipt = DeliveryReceipt

// DeliveryReceipt contains only bounded acknowledgement metadata. Response
// bodies are deliberately represented by a digest, never persisted verbatim.
type DeliveryReceipt struct {
	ReceiverID     string    `json:"receiver_id,omitempty"`
	EventDedupeKey string    `json:"event_dedupe_key"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
	StatusCode     int       `json:"status_code,omitempty"`
	ResponseDigest string    `json:"response_digest,omitempty"`
}

type durableDelivery struct {
	DeliveryRecord
	Payload []byte `json:"payload"`
}

// EnqueueDelivery atomically inserts one delivery and its
// (subscription,event_dedupe_key) index entry. Re-enqueueing the same pair is
// idempotent and returns the original delivery with reused=true.
func (j *Journal) EnqueueDelivery(ctx context.Context, request DeliveryRequest) (DeliveryRecord, bool, error) {
	if j.readOnly {
		return DeliveryRecord{}, false, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return DeliveryRecord{}, false, err
	}
	request.SubscriptionID = strings.TrimSpace(request.SubscriptionID)
	request.EventID = strings.TrimSpace(request.EventID)
	request.EventDedupeKey = strings.TrimSpace(request.EventDedupeKey)
	request.ExecutionID = strings.TrimSpace(request.ExecutionID)
	request.DeliveryID = strings.TrimSpace(request.DeliveryID)
	if err := validateDeliveryRequest(request); err != nil {
		return DeliveryRecord{}, false, err
	}
	var result DeliveryRecord
	var reused bool
	err := j.db.Update(func(tx *bbolt.Tx) error {
		var txErr error
		result, reused, txErr = enqueueDeliveryTx(tx, request, j.clock().UTC(), j.generator)
		return txErr
	})
	return result, reused, err
}

// EnqueueCallback is an alias retained for callers that use callback rather
// than delivery terminology.
func (j *Journal) EnqueueCallback(ctx context.Context, request DeliveryRequest) (DeliveryRecord, bool, error) {
	return j.EnqueueDelivery(ctx, request)
}

// EnqueueEventDelivery is a compact adapter for event producers that already
// have the five durable projections separated rather than in a request value.
func (j *Journal) EnqueueEventDelivery(ctx context.Context, subscriptionID, eventID, eventDedupeKey string, payload []byte, expiresAt time.Time) (DeliveryRecord, bool, error) {
	return j.EnqueueDelivery(ctx, DeliveryRequest{SubscriptionID: subscriptionID, EventID: eventID, EventDedupeKey: eventDedupeKey, Payload: payload, ExpiresAt: expiresAt})
}

func (j *Journal) Enqueue(ctx context.Context, request DeliveryRequest) (DeliveryRecord, bool, error) {
	return j.EnqueueDelivery(ctx, request)
}

// enqueueDeliveryTx is the transaction-level primitive shared by the public
// enqueue API and AppendEvent's subscription fan-out hook.
func enqueueDeliveryTx(tx *bbolt.Tx, request DeliveryRequest, journalNow time.Time, generator ids.Generator) (DeliveryRecord, bool, error) {
	if err := validateDeliveryRequest(request); err != nil {
		return DeliveryRecord{}, false, err
	}
	outbox, err := ensureJournalBucket(tx, bOutbox)
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	dedupe, err := ensureJournalBucket(tx, bOutboxDedupe)
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	if tx.Bucket(bSubscriptions) == nil || tx.Bucket(bSubscriptions).Get([]byte(request.SubscriptionID)) == nil {
		return DeliveryRecord{}, false, ErrSubscriptionNotFound
	}
	key := deliveryDedupeKey(request.SubscriptionID, request.EventDedupeKey)
	if existingID := dedupe.Get([]byte(key)); existingID != nil {
		raw := outbox.Get(existingID)
		if raw == nil {
			return DeliveryRecord{}, false, fmt.Errorf("%w: dedupe target missing", ErrCorrupt)
		}
		existing, err := decodeDelivery(raw)
		if err != nil {
			return DeliveryRecord{}, false, err
		}
		if existing.EventID != request.EventID || (existing.ExecutionID != "" && request.ExecutionID != "" && existing.ExecutionID != request.ExecutionID) || !bytes.Equal(existing.Payload, request.Payload) {
			return DeliveryRecord{}, false, fmt.Errorf("%w: dedupe key reused with different event", ErrDeliveryConflict)
		}
		return existing, true, nil
	}
	now := request.CreatedAt.UTC()
	if now.IsZero() {
		now = journalNow.UTC()
	}
	expires := request.ExpiresAt.UTC()
	if expires.IsZero() {
		ttl := request.TTL
		if ttl <= 0 {
			ttl = defaultDeliveryTTL
		}
		expires = now.Add(ttl)
	}
	if !expires.After(now) {
		return DeliveryRecord{}, false, fmt.Errorf("%w: delivery expires_at must be after created_at", ErrDeliveryConflict)
	}
	next := request.NextAttemptAt.UTC()
	if next.IsZero() {
		next = now
	}
	deliveryID := strings.TrimSpace(request.DeliveryID)
	if deliveryID == "" {
		deliveryID, err = newDeliveryID(tx, generator)
		if err != nil {
			return DeliveryRecord{}, false, err
		}
	} else if outbox.Get([]byte(deliveryID)) != nil {
		return DeliveryRecord{}, false, fmt.Errorf("%w: delivery ID %q already exists", ErrDeliveryConflict, deliveryID)
	}
	record := DeliveryRecord{ID: deliveryID, ExecutionID: strings.TrimSpace(request.ExecutionID), SubscriptionID: request.SubscriptionID, EventID: request.EventID, EventDedupeKey: request.EventDedupeKey, NextAttemptAt: next, ExpiresAt: expires, CreatedAt: now, State: callback.DeliveryPending, Payload: append([]byte(nil), request.Payload...)}
	encoded, err := encodeDelivery(record)
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	if err := outbox.Put([]byte(deliveryID), encoded); err != nil {
		return DeliveryRecord{}, false, err
	}
	if err := dedupe.Put([]byte(key), []byte(deliveryID)); err != nil {
		return DeliveryRecord{}, false, err
	}
	return record, false, nil
}

func (j *Journal) GetDelivery(ctx context.Context, id string) (DeliveryRecord, error) {
	if err := ctx.Err(); err != nil {
		return DeliveryRecord{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return DeliveryRecord{}, ErrDeliveryNotFound
	}
	if _, err := ids.ParseDeliveryID(id); err != nil {
		return DeliveryRecord{}, fmt.Errorf("%w: invalid delivery ID: %v", ErrDeliveryConflict, err)
	}
	var result DeliveryRecord
	err := j.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bOutbox)
		if bucket == nil {
			return ErrDeliveryNotFound
		}
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return ErrDeliveryNotFound
		}
		var err error
		result, err = decodeDelivery(raw)
		return err
	})
	return result, err
}

// ListDueDeliveries returns pending/retry records whose next attempt is due.
// Expired records are included so a scheduler can durably dead-letter them.
// Ordering is by next-attempt time, creation time, and delivery ID.
func (j *Journal) ListDueDeliveries(ctx context.Context, now time.Time, limit int) ([]DeliveryRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = j.clock().UTC()
	}
	if limit < 0 {
		return nil, errors.New("delivery limit cannot be negative")
	}
	result, err := j.listDeliveries(ctx, func(item DeliveryRecord) bool {
		return (item.State == callback.DeliveryPending || item.State == callback.DeliveryRetry) && (item.NextAttemptAt.IsZero() || !item.NextAttemptAt.After(now))
	}, limit)
	return result, err
}

// ListPendingDeliveries returns every pending/retry record, including not-yet-
// due and expired records. This is the projection used by a supervisor so it
// can choose between skip, retry, and dead-letter while retaining TTL state.
func (j *Journal) ListPendingDeliveries(ctx context.Context) ([]DeliveryRecord, error) {
	return j.listDeliveries(ctx, func(item DeliveryRecord) bool {
		return item.State == callback.DeliveryPending || item.State == callback.DeliveryRetry
	}, 0)
}

// ListPending is a short alias used by small adapters.
func (j *Journal) ListPending(ctx context.Context) ([]DeliveryRecord, error) {
	return j.ListPendingDeliveries(ctx)
}

// ListDuePending is the explicit pending spelling of ListDueDeliveries.
func (j *Journal) ListDuePending(ctx context.Context, now time.Time, limit int) ([]DeliveryRecord, error) {
	return j.ListDueDeliveries(ctx, now, limit)
}

// PendingDeliveries is a concise alias for ListPendingDeliveries.
func (j *Journal) PendingDeliveries(ctx context.Context) ([]DeliveryRecord, error) {
	return j.ListPendingDeliveries(ctx)
}

func (j *Journal) listDeliveries(ctx context.Context, include func(DeliveryRecord) bool, limit int) ([]DeliveryRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]DeliveryRecord, 0)
	err := j.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bOutbox)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, raw []byte) error {
			item, err := decodeDelivery(raw)
			if err != nil {
				return err
			}
			if include(item) {
				result = append(result, item)
			}
			return nil
		})
	})
	sort.Slice(result, func(i, k int) bool {
		if !result[i].NextAttemptAt.Equal(result[k].NextAttemptAt) {
			return result[i].NextAttemptAt.Before(result[k].NextAttemptAt)
		}
		if !result[i].CreatedAt.Equal(result[k].CreatedAt) {
			return result[i].CreatedAt.Before(result[k].CreatedAt)
		}
		return result[i].ID < result[k].ID
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, err
}

// AckDelivery records receipt metadata and is idempotent for a delivery ID.
func (j *Journal) AckDelivery(ctx context.Context, id string, receipt DeliveryReceipt) error {
	if j.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.db.Update(func(tx *bbolt.Tx) error {
		item, _, err := loadDelivery(tx, id)
		if err != nil {
			return err
		}
		if item.State == callback.DeliveryAcked {
			item.AttemptInFlight = false
			if receipt == (DeliveryReceipt{}) {
				return stopSubscriptionAfterTerminalAckTx(tx, item, j.clock().UTC())
			}
			ackNow := j.clock().UTC()
			if item.Receipt != nil && receipt.AcknowledgedAt.IsZero() {
				ackNow = item.Receipt.AcknowledgedAt
			}
			normalized := normalizeReceipt(receipt, item.EventDedupeKey, ackNow)
			if item.Receipt != nil {
				if *item.Receipt != *normalized {
					return fmt.Errorf("%w: acknowledgement metadata differs", ErrDeliveryConflict)
				}
				return stopSubscriptionAfterTerminalAckTx(tx, item, ackNow)
			}
			if item.Receipt == nil {
				item.Receipt = normalized
				receipts, err := ensureJournalBucket(tx, bOutboxReceipts)
				if err != nil {
					return err
				}
				if err := receipts.Put([]byte(item.ID), mustJSON(*normalized)); err != nil {
					return err
				}
				encoded, err := encodeDelivery(item)
				if err != nil {
					return err
				}
				if err := tx.Bucket(bOutbox).Put([]byte(item.ID), encoded); err != nil {
					return err
				}
				return stopSubscriptionAfterTerminalAckTx(tx, item, ackNow)
			}
			return nil
		}
		if item.State == callback.DeliveryDeadLetter || item.State == callback.DeliveryPaused {
			return fmt.Errorf("%w: delivery is %s", ErrDeliveryConflict, item.State)
		}
		normalized := normalizeReceipt(receipt, item.EventDedupeKey, j.clock().UTC())
		receipt = *normalized
		item.State = callback.DeliveryAcked
		item.AttemptInFlight = false
		item.NextAttemptAt = time.Time{}
		item.Receipt = &receipt
		receipts, err := ensureJournalBucket(tx, bOutboxReceipts)
		if err != nil {
			return err
		}
		if err := receipts.Put([]byte(item.ID), mustJSON(receipt)); err != nil {
			return err
		}
		encoded, err := encodeDelivery(item)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bOutbox).Put([]byte(item.ID), encoded); err != nil {
			return err
		}
		return stopSubscriptionAfterTerminalAckTx(tx, item, receipt.AcknowledgedAt)
	})
}

func stopSubscriptionAfterTerminalAckTx(tx *bbolt.Tx, item DeliveryRecord, acknowledgedAt time.Time) error {
	var envelope callback.Envelope
	if err := json.Unmarshal(item.Payload, &envelope); err != nil {
		return nil
	}
	// EnqueueDelivery is a public low-level API and permits bounded generic JSON.
	// Only subscription fan-out creates the callback envelope that can drive the
	// automatic terminal transition.
	if envelope.SchemaVersion != 1 || envelope.SubscriptionID != item.SubscriptionID || len(envelope.Event) == 0 {
		return nil
	}
	var event model.Event
	if err := json.Unmarshal(envelope.Event, &event); err != nil {
		return corrupt(fmt.Errorf("decode acknowledged event: %w", err))
	}
	if event.Kind != model.EventTerminal {
		return nil
	}
	bucket := tx.Bucket(bSubscriptions)
	if bucket == nil {
		return fmt.Errorf("%w: acknowledged subscription bucket is missing", ErrCorrupt)
	}
	raw := bucket.Get([]byte(item.SubscriptionID))
	if raw == nil {
		return fmt.Errorf("%w: acknowledged subscription is missing", ErrCorrupt)
	}
	var record SubscriptionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return corrupt(err)
	}
	if !record.Subscription.AutoExpireOnTerminal || record.Subscription.State != subscription.StateActive {
		return nil
	}
	record.Subscription.State = subscription.StateStopped
	record.Revision++
	if acknowledgedAt.IsZero() {
		acknowledgedAt = time.Now().UTC()
	}
	record.UpdatedAt = acknowledgedAt.UTC()
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(item.SubscriptionID), encoded)
}

// Ack is the supervisor-friendly no-metadata form.
func (j *Journal) Ack(ctx context.Context, id string) error {
	return j.AckDelivery(ctx, id, DeliveryReceipt{})
}

func (j *Journal) MarkAcknowledged(ctx context.Context, id string) error {
	return j.Ack(ctx, id)
}

func (j *Journal) GetDeliveryReceipt(ctx context.Context, id string) (DeliveryReceipt, error) {
	if err := ctx.Err(); err != nil {
		return DeliveryReceipt{}, err
	}
	var receipt DeliveryReceipt
	if _, err := ids.ParseDeliveryID(strings.TrimSpace(id)); err != nil {
		return DeliveryReceipt{}, fmt.Errorf("%w: invalid delivery ID: %v", ErrDeliveryConflict, err)
	}
	err := j.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bOutboxReceipts)
		if bucket == nil {
			return ErrDeliveryNotFound
		}
		raw := bucket.Get([]byte(strings.TrimSpace(id)))
		if raw == nil {
			return ErrDeliveryNotFound
		}
		return json.Unmarshal(raw, &receipt)
	})
	return receipt, err
}

func (j *Journal) GetReceipt(ctx context.Context, id string) (DeliveryReceipt, error) {
	return j.GetDeliveryReceipt(ctx, id)
}

// RetryDelivery updates the durable attempt schedule. Repeating a retry with
// the same schedule/reason is idempotent.
func (j *Journal) RetryDelivery(ctx context.Context, id string, next time.Time, reason string) error {
	if j.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.db.Update(func(tx *bbolt.Tx) error {
		item, _, err := loadDelivery(tx, id)
		if err != nil {
			return err
		}
		if item.State == callback.DeliveryRetry && item.NextAttemptAt.Equal(next.UTC()) && item.LastError == sanitizeDeliveryReason(reason) {
			return nil
		}
		if item.State == callback.DeliveryAcked || item.State == callback.DeliveryDeadLetter {
			return fmt.Errorf("%w: delivery is %s", ErrDeliveryConflict, item.State)
		}
		if item.AttemptInFlight {
			// The failed in-flight attempt has now been durably classified;
			// preserve its number while allowing the next scheduled attempt to
			// receive a fresh nonce/timestamp.
			item.AttemptInFlight = false
		} else {
			// Preserve compatibility with stores/supervisors that do not use
			// the optional begin-attempt boundary.
			item.Attempts++
		}
		item.State = callback.DeliveryRetry
		item.NextAttemptAt = next.UTC()
		item.LastError = sanitizeDeliveryReason(reason)
		encoded, err := encodeDelivery(item)
		if err != nil {
			return err
		}
		return tx.Bucket(bOutbox).Put([]byte(item.ID), encoded)
	})
}

// Retry implements the method shape used by supervisor.Outbox.
func (j *Journal) Retry(ctx context.Context, id string, next time.Time, reason string) error {
	return j.RetryDelivery(ctx, id, next, reason)
}

// BeginDeliveryAttempt durably materializes the wire envelope before any
// network or command side effect. If the process crashes while the attempt is
// in flight, a subsequent call returns the same cached payload and metadata;
// it does not mint another nonce. RetryDelivery clears AttemptInFlight before
// the next scheduled call, at which point a fresh attempt is generated.
func (j *Journal) BeginDeliveryAttempt(ctx context.Context, id string) (DeliveryRecord, error) {
	if j.readOnly {
		return DeliveryRecord{}, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return DeliveryRecord{}, err
	}
	var result DeliveryRecord
	err := j.db.Update(func(tx *bbolt.Tx) error {
		item, _, err := loadDelivery(tx, id)
		if err != nil {
			return err
		}
		if item.State == callback.DeliveryAcked || item.State == callback.DeliveryDeadLetter || item.State == callback.DeliveryPaused {
			return fmt.Errorf("%w: delivery is %s", ErrDeliveryConflict, item.State)
		}
		now := j.clock().UTC()
		if item.AttemptInFlight {
			var inFlight callback.Envelope
			if err := json.Unmarshal(item.Payload, &inFlight); err != nil {
				return fmt.Errorf("%w: in-flight callback envelope: %v", ErrDeliveryConflict, err)
			}
			if inFlight.DeliveryID != item.ID || inFlight.SubscriptionID != item.SubscriptionID || inFlight.EventID != item.EventID || inFlight.EventDedupeKey != item.EventDedupeKey {
				return fmt.Errorf("%w: in-flight callback envelope identity does not match durable delivery", ErrDeliveryConflict)
			}
			if inFlight.ExpiresAt.After(now) {
				result = item
				return nil
			}
			// A process may remain down past the wire freshness window. The
			// cached attempt can no longer receive a valid replay ACK; roll it
			// into a new scheduled attempt while retaining delivery identity.
			item.AttemptInFlight = false
		}
		if !item.ExpiresAt.IsZero() && !now.Before(item.ExpiresAt) {
			return fmt.Errorf("%w: delivery expired", ErrDeliveryConflict)
		}
		var base callback.Envelope
		if err := json.Unmarshal(item.Payload, &base); err != nil {
			return fmt.Errorf("%w: callback envelope: %v", ErrDeliveryConflict, err)
		}
		if base.DeliveryID != item.ID || base.SubscriptionID != item.SubscriptionID || base.EventID != item.EventID || base.EventDedupeKey != item.EventDedupeKey {
			return fmt.Errorf("%w: callback envelope identity does not match durable delivery", ErrDeliveryConflict)
		}
		fresh, err := callback.ReissueEnvelope(base, item.Attempts+1, now, item.ExpiresAt)
		if err != nil {
			return fmt.Errorf("%w: callback attempt: %v", ErrDeliveryConflict, err)
		}
		payload, err := fresh.CanonicalBody()
		if err != nil {
			return fmt.Errorf("%w: callback envelope encoding: %v", ErrDeliveryConflict, err)
		}
		item.Payload = payload
		item.Attempts++
		item.AttemptInFlight = true
		item.State = callback.DeliveryPending
		encoded, err := encodeDelivery(item)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bOutbox).Put([]byte(item.ID), encoded); err != nil {
			return err
		}
		result = item
		return nil
	})
	return result, err
}

// BeginAttempt is the short name used by supervisor/runtime bridge adapters.
func (j *Journal) BeginAttempt(ctx context.Context, id string) (DeliveryRecord, error) {
	return j.BeginDeliveryAttempt(ctx, id)
}

func (j *Journal) ScheduleRetry(ctx context.Context, id string, next time.Time, reason string) error {
	return j.Retry(ctx, id, next, reason)
}

func (j *Journal) PauseDelivery(ctx context.Context, id, reason string) error {
	if j.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.db.Update(func(tx *bbolt.Tx) error {
		item, _, err := loadDelivery(tx, id)
		if err != nil {
			return err
		}
		if item.State == callback.DeliveryPaused {
			return nil
		}
		if item.State == callback.DeliveryAcked || item.State == callback.DeliveryDeadLetter {
			return fmt.Errorf("%w: delivery is %s", ErrDeliveryConflict, item.State)
		}
		item.State = callback.DeliveryPaused
		item.AttemptInFlight = false
		item.PausedAt = j.clock().UTC()
		item.NextAttemptAt = time.Time{}
		item.LastError = sanitizeDeliveryReason(reason)
		encoded, err := encodeDelivery(item)
		if err != nil {
			return err
		}
		return tx.Bucket(bOutbox).Put([]byte(item.ID), encoded)
	})
}

func (j *Journal) Pause(ctx context.Context, id, reason string) error {
	return j.PauseDelivery(ctx, id, reason)
}

func (j *Journal) DeadLetterDelivery(ctx context.Context, id, reason string) error {
	if j.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.db.Update(func(tx *bbolt.Tx) error {
		outbox, err := ensureJournalBucket(tx, bOutbox)
		if err != nil {
			return err
		}
		dead, err := ensureJournalBucket(tx, bOutboxDead)
		if err != nil {
			return err
		}
		id = strings.TrimSpace(id)
		if _, err := ids.ParseDeliveryID(id); err != nil {
			return fmt.Errorf("%w: invalid delivery ID: %v", ErrDeliveryConflict, err)
		}
		raw := outbox.Get([]byte(id))
		if raw == nil {
			return ErrDeliveryNotFound
		}
		item, err := decodeDelivery(raw)
		if err != nil {
			return err
		}
		if item.State == callback.DeliveryDeadLetter {
			return nil
		}
		if item.State == callback.DeliveryAcked {
			return fmt.Errorf("%w: delivery already acknowledged", ErrDeliveryConflict)
		}
		item.State = callback.DeliveryDeadLetter
		item.AttemptInFlight = false
		item.NextAttemptAt = time.Time{}
		item.DeadLetterAt = j.clock().UTC()
		item.DeadLetterWhy = sanitizeDeliveryReason(reason)
		item.LastError = item.DeadLetterWhy
		encoded, err := encodeDelivery(item)
		if err != nil {
			return err
		}
		if err := outbox.Put([]byte(item.ID), encoded); err != nil {
			return err
		}
		return dead.Put([]byte(item.ID), encoded)
	})
}

func (j *Journal) DeadLetter(ctx context.Context, id, reason string) error {
	return j.DeadLetterDelivery(ctx, id, reason)
}

func (j *Journal) MarkDeadLetter(ctx context.Context, id, reason string) error {
	return j.DeadLetter(ctx, id, reason)
}

func validateDeliveryRequest(request DeliveryRequest) error {
	if strings.TrimSpace(request.SubscriptionID) == "" || strings.TrimSpace(request.EventID) == "" || strings.TrimSpace(request.EventDedupeKey) == "" {
		return fmt.Errorf("%w: subscription, event, and dedupe IDs are required", ErrDeliveryConflict)
	}
	if _, err := ids.ParseSubscriptionID(strings.TrimSpace(request.SubscriptionID)); err != nil {
		return fmt.Errorf("%w: invalid subscription ID: %v", ErrDeliveryConflict, err)
	}
	if _, err := ids.ParseEventID(strings.TrimSpace(request.EventID)); err != nil {
		return fmt.Errorf("%w: invalid event ID: %v", ErrDeliveryConflict, err)
	}
	if request.ExecutionID != "" {
		if _, err := ids.ParseExecutionID(strings.TrimSpace(request.ExecutionID)); err != nil {
			return fmt.Errorf("%w: invalid execution ID: %v", ErrDeliveryConflict, err)
		}
	}
	if request.DeliveryID != "" {
		if _, err := ids.ParseDeliveryID(strings.TrimSpace(request.DeliveryID)); err != nil {
			return fmt.Errorf("%w: invalid delivery ID: %v", ErrDeliveryConflict, err)
		}
	}
	if !deliveryDedupePattern.MatchString(strings.TrimSpace(request.EventDedupeKey)) {
		return fmt.Errorf("%w: event dedupe key must be sha256 hex", ErrDeliveryConflict)
	}
	if len(request.Payload) == 0 {
		return fmt.Errorf("%w: payload is required", ErrDeliveryConflict)
	}
	if len(request.Payload) > MaxOutboxPayload {
		return fmt.Errorf("%w: payload exceeds %d bytes", ErrDeliveryConflict, MaxOutboxPayload)
	}
	if badOutboxPayload(request.Payload) {
		return fmt.Errorf("%w: payload contains a secret or raw log field", ErrDeliveryConflict)
	}
	return nil
}

func badOutboxPayload(payload []byte) bool {
	var value any
	dec := json.NewDecoder(bytes.NewReader(payload))
	if err := dec.Decode(&value); err != nil {
		return true
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return true
	} else if !errors.Is(err, io.EOF) {
		return true
	}
	return sensitiveValue(value)
}

func sensitiveValue(value any) bool {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			key = strings.ToLower(strings.TrimSpace(key))
			switch key {
			case "prompt", "raw_prompt", "transcript", "reasoning", "chain_of_thought", "stdout", "stderr", "raw_output", "token_delta", "tokens", "logs", "log_body", "secret", "secrets", "password", "passwd", "authorization", "cookie", "credential", "credentials":
				return true
			}
			if sensitiveValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range item {
			if sensitiveValue(child) {
				return true
			}
		}
	}
	return false
}

func deliveryDedupeKey(subscriptionID, eventDedupe string) string {
	return strings.TrimSpace(subscriptionID) + "\x00" + strings.TrimSpace(eventDedupe)
}

func newDeliveryID(tx *bbolt.Tx, generator ids.Generator) (string, error) {
	if generator == nil {
		return "", errors.New("delivery ID generator is required")
	}
	bucket := tx.Bucket(bOutbox)
	for range 16 {
		id, err := generator.New(ids.TypeDelivery)
		if err != nil {
			return "", err
		}
		if bucket.Get([]byte(id.String())) == nil {
			return id.String(), nil
		}
	}
	return "", fmt.Errorf("%w: exhausted delivery ID collisions", ErrDeliveryConflict)
}

func loadDelivery(tx *bbolt.Tx, id string) (DeliveryRecord, []byte, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return DeliveryRecord{}, nil, ErrDeliveryNotFound
	}
	if _, err := ids.ParseDeliveryID(id); err != nil {
		return DeliveryRecord{}, nil, fmt.Errorf("%w: invalid delivery ID: %v", ErrDeliveryConflict, err)
	}
	bucket := tx.Bucket(bOutbox)
	if bucket == nil {
		return DeliveryRecord{}, nil, ErrDeliveryNotFound
	}
	raw := bucket.Get([]byte(id))
	if raw == nil {
		return DeliveryRecord{}, nil, ErrDeliveryNotFound
	}
	item, err := decodeDelivery(raw)
	return item, raw, err
}

func encodeDelivery(item DeliveryRecord) ([]byte, error) {
	return json.Marshal(durableDelivery{DeliveryRecord: item, Payload: append([]byte(nil), item.Payload...)})
}

func decodeDelivery(raw []byte) (DeliveryRecord, error) {
	var item durableDelivery
	if err := json.Unmarshal(raw, &item); err != nil {
		return DeliveryRecord{}, corrupt(err)
	}
	if item.ID == "" || item.SubscriptionID == "" || item.EventID == "" || item.EventDedupeKey == "" || item.State == "" {
		return DeliveryRecord{}, fmt.Errorf("%w: malformed delivery", ErrCorrupt)
	}
	if _, err := ids.ParseDeliveryID(item.ID); err != nil {
		return DeliveryRecord{}, fmt.Errorf("%w: malformed delivery ID: %v", ErrCorrupt, err)
	}
	if _, err := ids.ParseSubscriptionID(item.SubscriptionID); err != nil {
		return DeliveryRecord{}, fmt.Errorf("%w: malformed subscription ID: %v", ErrCorrupt, err)
	}
	if _, err := ids.ParseEventID(item.EventID); err != nil {
		return DeliveryRecord{}, fmt.Errorf("%w: malformed event ID: %v", ErrCorrupt, err)
	}
	if item.ExecutionID != "" {
		if _, err := ids.ParseExecutionID(item.ExecutionID); err != nil {
			return DeliveryRecord{}, fmt.Errorf("%w: malformed execution ID: %v", ErrCorrupt, err)
		}
	}
	if !deliveryDedupePattern.MatchString(item.EventDedupeKey) {
		return DeliveryRecord{}, fmt.Errorf("%w: malformed event dedupe key", ErrCorrupt)
	}
	if len(item.Payload) == 0 || len(item.Payload) > MaxOutboxPayload {
		return DeliveryRecord{}, fmt.Errorf("%w: malformed delivery payload", ErrCorrupt)
	}
	item.DeliveryRecord.Payload = append([]byte(nil), item.Payload...)
	item.Payload = nil
	return item.DeliveryRecord, nil
}

func normalizeReceipt(receipt DeliveryReceipt, dedupe string, now time.Time) *DeliveryReceipt {
	if receipt.EventDedupeKey == "" {
		receipt.EventDedupeKey = dedupe
	}
	if receipt.AcknowledgedAt.IsZero() {
		receipt.AcknowledgedAt = now.UTC()
	} else {
		receipt.AcknowledgedAt = receipt.AcknowledgedAt.UTC()
	}
	receipt.ReceiverID = sanitizeDeliveryReason(receipt.ReceiverID)
	receipt.ResponseDigest = sanitizeDeliveryReason(receipt.ResponseDigest)
	return &receipt
}

func sanitizeDeliveryReason(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			if r == '\n' || r == '\r' || r == '\t' {
				b.WriteByte(' ')
			}
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxReceiptText {
			break
		}
	}
	return b.String()
}

func mustJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}
