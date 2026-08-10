package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/ids"
)

func TestBeginDeliveryAttemptCachesCrashResendAndRefreshesScheduledRetry(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	current := now
	j.clock = func() time.Time { return current }
	subID := durableTestID(ids.TypeSubscription, 910)
	sub := durableTestSubscription(subID)
	if _, _, err := j.PutSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	eventID := durableTestID(ids.TypeEvent, 911)
	dedupe := "sha256:" + strings.Repeat("a", 64)
	base, err := callback.NewEnvelope(durableTestID(ids.TypeDelivery, 912), subID, eventID, dedupe, "initial", 1, now, now.Add(time.Hour), map[string]any{"state": "completed"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base.CanonicalBody()
	if err != nil {
		t.Fatal(err)
	}
	delivery, _, err := j.EnqueueDelivery(ctx, DeliveryRequest{DeliveryID: base.DeliveryID, SubscriptionID: subID, EventID: eventID, EventDedupeKey: dedupe, Payload: payload, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	first, err := j.BeginDeliveryAttempt(ctx, delivery.ID)
	if err != nil {
		t.Fatal(err)
	}
	var firstEnvelope callback.Envelope
	if err := json.Unmarshal(first.Payload, &firstEnvelope); err != nil {
		t.Fatal(err)
	}
	if first.Attempts != 1 || !first.AttemptInFlight || firstEnvelope.Attempt != 1 || firstEnvelope.Nonce == "initial" {
		t.Fatalf("first attempt=%+v envelope=%+v", first, firstEnvelope)
	}
	crashResend, err := j.BeginDeliveryAttempt(ctx, delivery.ID)
	if err != nil {
		t.Fatal(err)
	}
	if crashResend.Attempts != first.Attempts || !crashResend.AttemptInFlight || string(crashResend.Payload) != string(first.Payload) {
		t.Fatal("in-flight restart did not reuse exact cached envelope")
	}
	current = current.Add(time.Second)
	if err := j.RetryDelivery(ctx, delivery.ID, current.Add(time.Second), "timeout"); err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Second)
	second, err := j.BeginDeliveryAttempt(ctx, delivery.ID)
	if err != nil {
		t.Fatal(err)
	}
	var secondEnvelope callback.Envelope
	if err := json.Unmarshal(second.Payload, &secondEnvelope); err != nil {
		t.Fatal(err)
	}
	if second.Attempts != 2 || !second.AttemptInFlight || secondEnvelope.Attempt != 2 || secondEnvelope.Nonce == firstEnvelope.Nonce || !secondEnvelope.SentAt.After(firstEnvelope.SentAt) {
		t.Fatalf("scheduled retry did not refresh metadata: record=%+v envelope=%+v first=%+v", second, secondEnvelope, firstEnvelope)
	}
	// An in-flight crash recovered after the five-minute wire freshness
	// window must mint a new attempt rather than resend an expired envelope.
	current = secondEnvelope.ExpiresAt.Add(time.Second)
	third, err := j.BeginDeliveryAttempt(ctx, delivery.ID)
	if err != nil {
		t.Fatal(err)
	}
	var thirdEnvelope callback.Envelope
	if err := json.Unmarshal(third.Payload, &thirdEnvelope); err != nil {
		t.Fatal(err)
	}
	if third.Attempts != 3 || thirdEnvelope.Attempt != 3 || thirdEnvelope.Nonce == secondEnvelope.Nonce || !thirdEnvelope.SentAt.After(secondEnvelope.SentAt) {
		t.Fatalf("expired in-flight attempt was not refreshed: record=%+v envelope=%+v", third, thirdEnvelope)
	}
}
