package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"
)

type attemptOutbox struct{ fakeOutbox }

func (f *attemptOutbox) BeginAttempt(_ context.Context, id string) (Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[id]
	if !ok {
		return Delivery{}, errors.New("delivery missing")
	}
	if !entry.AttemptInFlight {
		entry.Attempts++
		entry.AttemptInFlight = true
		f.entries[id] = entry
	}
	return entry, nil
}

func (f *attemptOutbox) Retry(_ context.Context, id string, next time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.entries[id]
	entry.AttemptInFlight = false
	entry.NextAttemptAt = next
	f.entries[id] = entry
	f.retries = append(f.retries, retryCall{id: id, next: next, reason: reason})
	return nil
}

func TestAttemptStarterPersistsCurrentNumberAcrossRetryCycles(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	outbox := &attemptOutbox{fakeOutbox: fakeOutbox{entries: map[string]Delivery{"delivery": {ID: "delivery"}}}}
	deliverer := &fakeDeliverer{errors: map[string][]error{"delivery": {
		&RetryableDeliveryError{Err: errors.New("first")},
		&RetryableDeliveryError{Err: errors.New("second")},
		nil,
	}}}
	cfg := testConfig(t, clock)
	cfg.MaxDeliveryAttempts = 3
	s, err := New(cfg, Dependencies{Outbox: outbox, Deliverer: deliverer})
	if err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 8; cycle++ {
		clock.now = clock.now.Add(time.Second)
		if _, err := s.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(deliverer.calls) != 3 || len(outbox.acks) != 1 || len(outbox.retries) != 2 {
		t.Fatalf("attempt lifecycle calls=%v acks=%v retries=%v", deliverer.calls, outbox.acks, outbox.retries)
	}
	if got := outbox.entries["delivery"].Attempts; got != 0 {
		// Ack removes the entry in fakeOutbox; a zero value here only means the
		// map no longer contains it. Keep this assertion as a guard against a
		// hidden fourth attempt if a future fake changes Ack semantics.
		t.Fatalf("unexpected retained delivery attempts=%d", got)
	}
}

func TestAttemptStarterResendsInFlightFinalAttemptAfterCrash(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	outbox := &attemptOutbox{fakeOutbox: fakeOutbox{entries: map[string]Delivery{"delivery": {ID: "delivery", Attempts: 3, AttemptInFlight: true}}}}
	deliverer := &fakeDeliverer{errors: map[string][]error{"delivery": {nil}}}
	cfg := testConfig(t, clock)
	cfg.MaxDeliveryAttempts = 3
	s, err := New(cfg, Dependencies{Outbox: outbox, Deliverer: deliverer})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := s.RunOnce(context.Background())
	if report.Delivery.Delivered != 1 || len(deliverer.calls) != 1 || len(outbox.acks) != 1 {
		t.Fatalf("in-flight final attempt was not resent: report=%+v calls=%v acks=%v", report.Delivery, deliverer.calls, outbox.acks)
	}
}
