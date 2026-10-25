package supervisor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// slowReprober advances the fake clock on every probe, standing in for an
// authority that takes its whole probe timeout to answer.
type slowReprober struct {
	clock *fakeClock
	cost  time.Duration
	calls []string
}

func (r *slowReprober) Reprobe(_ context.Context, execution Execution) (ProbeResult, error) {
	r.calls = append(r.calls, execution.ID)
	r.clock.mu.Lock()
	r.clock.now = r.clock.now.Add(r.cost)
	r.clock.mu.Unlock()
	return ProbeResult{State: "running"}, nil
}

// One slow execution must not hold back delivery or starve the executions
// after it: the recovery pass stops at its budget, delivery still runs, and
// the next cycle resumes after the last execution examined.
func TestRecoveryBudgetRotatesAndDeliveryStillRuns(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	executions := &fakeExecutions{
		items:    []Execution{{ID: "a", State: "running"}, {ID: "b", State: "running"}, {ID: "c", State: "running"}, {ID: "d", State: "running"}},
		results:  map[string]ProbeResult{},
		applyErr: map[string]error{},
	}
	reprober := &slowReprober{clock: clock, cost: 20 * time.Second}
	outbox := &fakeOutbox{entries: map[string]Delivery{"x": {ID: "x", ExecutionID: "z", SubscriptionID: "sub"}}}
	deliverer := &fakeDeliverer{errors: map[string][]error{}}
	cfg := testConfig(t, clock)
	cfg.RecoveryBudget = 30 * time.Second
	s, err := New(cfg, Dependencies{Executions: executions, Reprober: reprober, Outbox: outbox, Deliverer: deliverer})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := s.RunOnce(context.Background())
	if report.Recovery.Probed != 2 || report.Recovery.Deferred != 2 {
		t.Fatalf("first cycle recovery = %+v, want 2 probed and 2 deferred", report.Recovery)
	}
	if report.Delivery.Delivered != 1 {
		t.Fatalf("delivery waited behind recovery: %+v", report.Delivery)
	}
	if health := s.Health(); health.LastCycleRecoveryDeferred != 2 || health.State == HealthDegraded {
		t.Fatalf("health after deferred recovery = %+v", health)
	}
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c", "d"}; !reflect.DeepEqual(reprober.calls, want) {
		t.Fatalf("probe order = %v, want %v (second cycle resumes after b)", reprober.calls, want)
	}
}

// A reprobe failure names the execution in the health error, and a later
// clean cycle restores healthy and clears the stale error.
func TestRecoveryErrorNamesExecutionAndHealthRecovers(t *testing.T) {
	clock := &fakeClock{now: time.Unix(10, 0)}
	executions := &fakeExecutions{
		items:    []Execution{{ID: "exec-bad", State: "running"}, {ID: "exec-good", State: "running"}},
		results:  map[string]ProbeResult{"exec-good": {State: "running"}},
		applyErr: map[string]error{},
	}
	reprober := &fakeReprober{results: executions.results, errors: map[string]error{"exec-bad": errors.New("get_execution failed: journal busy")}}
	s, err := New(testConfig(t, clock), Dependencies{Executions: executions, Reprober: reprober})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := s.RunOnce(context.Background())
	if report.Recovery.Applied != 1 {
		t.Fatalf("good execution was not recovered: %+v", report.Recovery)
	}
	health := s.Health()
	if health.State != HealthDegraded || !strings.Contains(health.LastError, "exec-bad") || !strings.Contains(health.LastError, "journal busy") || health.LastErrorAt.IsZero() {
		t.Fatalf("health = %+v, want degraded naming exec-bad and its cause", health)
	}
	delete(reprober.errors, "exec-bad")
	clock.mu.Lock()
	clock.now = clock.now.Add(time.Hour) // past the probe backoff
	clock.mu.Unlock()
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if health := s.Health(); health.State != HealthHealthy || health.LastError != "" || health.LastErrorAt.IsZero() {
		t.Fatalf("health after a clean cycle = %+v, want healthy with the error cleared", health)
	}
}

// A destination that keeps refusing (for example a group-readable file
// destination directory) must show up in health, naming the delivery,
// subscription and execution, instead of retrying silently until expiry.
func TestRetryingDeliveryDegradesHealthUntilDelivered(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	outbox := &fakeOutbox{entries: map[string]Delivery{
		"del-1": {ID: "del-1", ExecutionID: "exec-1", SubscriptionID: "sub-1", ExpiresAt: clock.now.Add(24 * time.Hour)},
	}}
	deliverer := &fakeDeliverer{errors: map[string][]error{"del-1": {&RetryableDeliveryError{Err: errors.New(`state directory "/x" must be owner-only`)}}}}
	s, err := New(testConfig(t, clock), Dependencies{Outbox: outbox, Deliverer: deliverer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	health := s.Health()
	if health.State != HealthDegraded || health.RetryingDeliveries != 1 {
		t.Fatalf("health after a failed delivery = %+v", health)
	}
	for _, want := range []string{"del-1", "sub-1", "exec-1", "owner-only"} {
		if !strings.Contains(health.LastDeliveryError, want) || !strings.Contains(health.LastError, want) {
			t.Fatalf("delivery error %q / %q does not name %s", health.LastDeliveryError, health.LastError, want)
		}
	}
	// Waiting out the backoff still counts as retrying.
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if health := s.Health(); health.State != HealthDegraded || health.RetryingDeliveries != 1 {
		t.Fatalf("health during backoff = %+v", health)
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(time.Hour)
	clock.mu.Unlock()
	report, _ := s.RunOnce(context.Background())
	if report.Delivery.Delivered != 1 {
		t.Fatalf("delivery after the destination recovered = %+v", report.Delivery)
	}
	if health := s.Health(); health.State != HealthHealthy || health.RetryingDeliveries != 0 || health.LastDeliveryError != "" || health.PendingDeliveries != 0 {
		t.Fatalf("health after delivery = %+v", health)
	}
}

func TestPermanentDeliveryFailureIsReported(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	outbox := &fakeOutbox{entries: map[string]Delivery{"del-1": {ID: "del-1", ExecutionID: "exec-1", SubscriptionID: "sub-1"}}}
	deliverer := &fakeDeliverer{errors: map[string][]error{"del-1": {&PermanentDeliveryError{Err: errors.New("subscription missing")}}}}
	s, err := New(testConfig(t, clock), Dependencies{Outbox: outbox, Deliverer: deliverer})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := s.RunOnce(context.Background())
	if report.Delivery.DeadLettered != 1 || len(report.Delivery.Failures) != 1 || report.Delivery.Failures[0].Action != "deliver" {
		t.Fatalf("permanent failure report = %+v", report.Delivery)
	}
	if health := s.Health(); health.State != HealthDegraded || !strings.Contains(health.LastDeliveryError, "del-1") {
		t.Fatalf("health = %+v", health)
	}
}
