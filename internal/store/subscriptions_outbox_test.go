package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

func durableTestSubscription(id string) subscription.Subscription {
	return subscription.Subscription{
		ID: id, Origin: durableTestID(ids.TypeHost, 100),
		Filter:      subscription.EventFilter{ExecutionIDs: []string{durableTestID(ids.TypeExecution, 91)}},
		Destination: callback.Destination{Kind: callback.DestinationStdout},
		State:       subscription.StateActive,
	}
}

func durableTestID(typ ids.Type, payload uint64) string {
	id, err := ids.FromPayload(typ, payload)
	if err != nil {
		panic(err)
	}
	return id.String()
}

func TestSubscriptionPutUpdateCASAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	generator := &sequenceGenerator{next: map[ids.Type]uint64{ids.TypeHost: 100, ids.TypeExecution: 2, ids.TypeEvent: 3, ids.TypeSubscription: 4, ids.TypeDelivery: 5}}
	now := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	j, err := Open(path, Options{Generator: generator, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	subID := durableTestID(ids.TypeSubscription, 100)
	created, reused, err := j.PutSubscription(ctx, durableTestSubscription(subID))
	if err != nil || reused || created.ID != subID {
		t.Fatalf("created=%#v reused=%v err=%v", created, reused, err)
	}
	if _, reused, err := j.PutSubscription(ctx, durableTestSubscription(subID)); err != nil || !reused {
		t.Fatalf("idempotent put reused=%v err=%v", reused, err)
	}
	changed := created
	changed.State = subscription.StatePaused
	if _, err := j.UpdateSubscription(ctx, changed, 0); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatalf("zero CAS err=%v", err)
	}
	record, err := j.GetSubscriptionRecord(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := j.UpdateSubscription(ctx, changed, record.Revision)
	if err != nil || updated.State != subscription.StatePaused {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	if _, err := j.UpdateSubscription(ctx, changed, record.Revision); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatalf("stale CAS err=%v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(path, Options{Generator: generator, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	got, err := j.GetSubscription(ctx, created.ID)
	if err != nil || got.State != subscription.StatePaused {
		t.Fatalf("restarted subscription=%#v err=%v", got, err)
	}
}

func TestOutboxAtomicDedupeConcurrencyAndDeterministicDueOrder(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	subID := durableTestID(ids.TypeSubscription, 100)
	if _, _, err := j.PutSubscription(ctx, durableTestSubscription(subID)); err != nil {
		t.Fatal(err)
	}
	eventOne := durableTestID(ids.TypeEvent, 200)
	deliveryOne := durableTestID(ids.TypeDelivery, 300)
	request := DeliveryRequest{DeliveryID: deliveryOne, SubscriptionID: subID, EventID: eventOne, EventDedupeKey: "sha256:" + strings.Repeat("1", 64), Payload: []byte(`{"event":"one"}`), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	const workers = 16
	results := make(chan DeliveryRecord, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, _, err := j.EnqueueDelivery(ctx, request)
			if err != nil {
				errs <- err
				return
			}
			results <- item
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for item := range results {
		if item.ID != request.DeliveryID || string(item.Payload) != string(request.Payload) {
			t.Fatalf("unexpected concurrent result=%#v", item)
		}
	}
	if pending, err := j.ListPendingDeliveries(ctx); err != nil || len(pending) != 1 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	second := request
	second.DeliveryID = durableTestID(ids.TypeDelivery, 301)
	second.EventID = durableTestID(ids.TypeEvent, 201)
	second.EventDedupeKey = "sha256:" + strings.Repeat("2", 64)
	second.NextAttemptAt = now.Add(-time.Second)
	second.CreatedAt = now.Add(time.Second)
	second.Payload = []byte(`{"event":"two"}`)
	if _, _, err := j.EnqueueDelivery(ctx, second); err != nil {
		t.Fatal(err)
	}
	due, err := j.ListDueDeliveries(ctx, now, 0)
	if err != nil || len(due) != 2 || due[0].ID != second.DeliveryID || due[1].ID != request.DeliveryID {
		t.Fatalf("due order=%#v err=%v", due, err)
	}
}

func TestOutboxReceiptRetryPauseDeadLetterAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	now := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	generator := &sequenceGenerator{next: map[ids.Type]uint64{ids.TypeHost: 100, ids.TypeExecution: 2, ids.TypeEvent: 3, ids.TypeSubscription: 4, ids.TypeDelivery: 5}}
	j, err := Open(path, Options{Generator: generator, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	subID := durableTestID(ids.TypeSubscription, 100)
	if _, _, err := j.PutSubscription(ctx, durableTestSubscription(subID)); err != nil {
		t.Fatal(err)
	}
	item, _, err := j.EnqueueDelivery(ctx, DeliveryRequest{DeliveryID: durableTestID(ids.TypeDelivery, 400), SubscriptionID: subID, EventID: durableTestID(ids.TypeEvent, 500), EventDedupeKey: "sha256:" + strings.Repeat("3", 64), Payload: []byte(`{"ok":true}`), CreatedAt: now, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	receipt := DeliveryReceipt{ReceiverID: "receiver-a", EventDedupeKey: item.EventDedupeKey, StatusCode: 204, ResponseDigest: "sha256:response"}
	if err := j.AckDelivery(ctx, item.ID, receipt); err != nil {
		t.Fatal(err)
	}
	if err := j.AckDelivery(ctx, item.ID, DeliveryReceipt{ReceiverID: receipt.ReceiverID, EventDedupeKey: receipt.EventDedupeKey, StatusCode: receipt.StatusCode, ResponseDigest: receipt.ResponseDigest}); err != nil {
		t.Fatalf("idempotent ack metadata err=%v", err)
	}
	if err := j.Ack(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	gotReceipt, err := j.GetDeliveryReceipt(ctx, item.ID)
	if err != nil || gotReceipt.ReceiverID != receipt.ReceiverID || gotReceipt.StatusCode != 204 {
		t.Fatalf("receipt=%#v err=%v", gotReceipt, err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(path, Options{Generator: generator, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if got, err := j.GetDelivery(ctx, item.ID); err != nil || got.State != callback.DeliveryAcked {
		t.Fatalf("restarted delivery=%#v err=%v", got, err)
	}
	if _, err := j.GetDeliveryReceipt(ctx, item.ID); err != nil {
		t.Fatalf("restarted receipt err=%v", err)
	}

	idsForTest := []struct{ delivery, event, dedupe string }{
		{durableTestID(ids.TypeDelivery, 401), durableTestID(ids.TypeEvent, 501), "sha256:" + strings.Repeat("4", 64)},
		{durableTestID(ids.TypeDelivery, 402), durableTestID(ids.TypeEvent, 502), "sha256:" + strings.Repeat("5", 64)},
		{durableTestID(ids.TypeDelivery, 403), durableTestID(ids.TypeEvent, 503), "sha256:" + strings.Repeat("6", 64)},
	}
	for _, item := range idsForTest {
		if _, _, err := j.EnqueueDelivery(ctx, DeliveryRequest{DeliveryID: item.delivery, SubscriptionID: subID, EventID: item.event, EventDedupeKey: item.dedupe, Payload: []byte(`{"ok":true}`), CreatedAt: now, TTL: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Retry(ctx, idsForTest[0].delivery, now.Add(time.Minute), "temporary network failure"); err != nil {
		t.Fatal(err)
	}
	if got, _ := j.GetDelivery(ctx, idsForTest[0].delivery); got.Attempts != 1 || got.State != callback.DeliveryRetry {
		t.Fatalf("retry state=%#v", got)
	}
	if err := j.PauseDelivery(ctx, idsForTest[1].delivery, "credentials unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := j.DeadLetter(ctx, idsForTest[2].delivery, "permanent destination error"); err != nil {
		t.Fatal(err)
	}
	if got, _ := j.GetDelivery(ctx, idsForTest[2].delivery); got.State != callback.DeliveryDeadLetter || got.DeadLetterWhy == "" {
		t.Fatalf("dead state=%#v", got)
	}
}

func TestSubscriptionFanoutUsesNormalizedEnvelopeAndExactFilters(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	execution, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	subID := durableTestID(ids.TypeSubscription, 800)
	sub := durableTestSubscription(subID)
	sub.Filter.ExecutionIDs = []string{execution.ID.String()}
	sub.Filter.Kinds = []string{string(model.EventStarted)}
	if _, _, err := j.PutSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	eventID := durableTestID(ids.TypeEvent, 801)
	event := model.Event{SchemaVersion: model.SchemaVersion, ID: ids.EventID(eventID), OriginHostID: execution.OriginHostID, ExecutionID: execution.ID, Sequence: 1, Ordering: model.OrderingObservation, Kind: model.EventStarted, Authority: execution.Authority, Adapter: execution.Adapter, ObservedAt: now, DedupeKey: hash('a'), DedupeVersion: 1, Payload: map[string]any{"accepted": true}}
	projection := []byte(`{"raw_projection":"must-not-ship"}`)
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		return enqueueMatchingSubscriptionsTx(tx, &event, projection, now, j.generator)
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := j.ListPendingDeliveries(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	var envelope callback.Envelope
	if err := json.Unmarshal(pending[0].Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SubscriptionID != subID || envelope.EventID != eventID || string(envelope.Event) == string(projection) || strings.Contains(string(envelope.Event), "raw_projection") {
		t.Fatalf("unexpected callback envelope=%s", pending[0].Payload)
	}
	if _, _, err := j.EnqueueDelivery(ctx, DeliveryRequest{DeliveryID: durableTestID(ids.TypeDelivery, 802), SubscriptionID: subID, EventID: eventID, EventDedupeKey: event.DedupeKey, Payload: pending[0].Payload, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if pending, err := j.ListPendingDeliveries(ctx); err != nil || len(pending) != 1 {
		t.Fatalf("dedupe fanout pending=%#v err=%v", pending, err)
	}
}

func TestLegacyDirectAuthorityFilterMatchesNativeEvent(t *testing.T) {
	now := time.Now().UTC()
	value := subscription.Subscription{State: subscription.StateActive, ExpiresAt: now.Add(time.Hour), Filter: subscription.EventFilter{Authority: "direct"}}
	event := &model.Event{Authority: model.AuthorityNative}
	if !matchesEventScope(value, event, now) {
		t.Fatal("legacy direct authority did not match native event")
	}
	value.Filter.Authority = "unknown"
	if matchesEventScope(value, event, now) {
		t.Fatal("unknown authority unexpectedly matched")
	}
}

func TestSubscriptionAutoExpiresOnlyAfterTerminalAcknowledgement(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	execution, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	autoID := durableTestID(ids.TypeSubscription, 820)
	auto := durableTestSubscription(autoID)
	auto.Filter.ExecutionIDs = []string{execution.ID.String()}
	auto.Filter.Kinds = []string{string(model.EventTerminal)}
	auto.AutoExpireOnTerminal = true
	if _, _, err := j.PutSubscription(ctx, auto); err != nil {
		t.Fatal(err)
	}
	keepID := durableTestID(ids.TypeSubscription, 821)
	keep := durableTestSubscription(keepID)
	keep.Filter.ExecutionIDs = []string{execution.ID.String()}
	keep.Filter.Kinds = []string{string(model.EventTerminal)}
	keep.AutoExpireOnTerminal = false
	if _, _, err := j.PutSubscription(ctx, keep); err != nil {
		t.Fatal(err)
	}
	terminalState := model.StateCompleted
	event := model.Event{SchemaVersion: model.SchemaVersion, ID: ids.EventID(durableTestID(ids.TypeEvent, 822)), OriginHostID: execution.OriginHostID, ExecutionID: execution.ID, Sequence: 1, Ordering: model.OrderingObservation, Kind: model.EventTerminal, State: &terminalState, Authority: execution.Authority, Adapter: execution.Adapter, ObservedAt: now, DedupeKey: hash('f'), DedupeVersion: 1, Payload: map[string]any{"status": "completed"}}
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		return enqueueMatchingSubscriptionsTx(tx, &event, []byte(`{"terminal":true}`), now, j.generator)
	}); err != nil {
		t.Fatal(err)
	}
	autoBeforeAck, err := j.GetSubscription(ctx, autoID)
	if err != nil || autoBeforeAck.State != subscription.StateActive {
		t.Fatalf("subscription stopped before acknowledgement=%#v err=%v", autoBeforeAck, err)
	}
	keepAfter, err := j.GetSubscription(ctx, keepID)
	if err != nil || keepAfter.State != subscription.StateActive {
		t.Fatalf("kept subscription=%#v err=%v", keepAfter, err)
	}
	pending, err := j.ListPendingDeliveries(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatalf("terminal deliveries=%#v err=%v", pending, err)
	}
	var autoDelivery string
	for _, delivery := range pending {
		if delivery.SubscriptionID == autoID {
			autoDelivery = delivery.ID
		}
	}
	if autoDelivery == "" {
		t.Fatal("auto-expiring subscription has no terminal delivery")
	}
	if err := j.AckDelivery(ctx, autoDelivery, DeliveryReceipt{}); err != nil {
		t.Fatal(err)
	}
	autoAfterAck, err := j.GetSubscription(ctx, autoID)
	if err != nil || autoAfterAck.State != subscription.StateStopped {
		t.Fatalf("acknowledged subscription=%#v err=%v", autoAfterAck, err)
	}
	keepAfter, err = j.GetSubscription(ctx, keepID)
	if err != nil || keepAfter.State != subscription.StateActive {
		t.Fatalf("kept subscription changed after another ack=%#v err=%v", keepAfter, err)
	}
}

func TestSubscriptionReadsExposeEffectiveExpiredState(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	value := durableTestSubscription(durableTestID(ids.TypeSubscription, 823))
	value.ExpiresAt = now.Add(time.Minute)
	if _, _, err := j.PutSubscription(ctx, value); err != nil {
		t.Fatal(err)
	}
	j.clock = func() time.Time { return now.Add(2 * time.Minute) }
	shown, err := j.GetSubscription(ctx, value.ID)
	if err != nil || shown.State != subscription.StateExpired {
		t.Fatalf("expired subscription=%#v err=%v", shown, err)
	}
	listed, err := j.ListSubscriptions(ctx)
	if err != nil || len(listed) != 1 || listed[0].State != subscription.StateExpired {
		t.Fatalf("expired subscription list=%#v err=%v", listed, err)
	}
}
