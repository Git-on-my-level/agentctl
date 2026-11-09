package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCycleRunsMaintenanceAndSurfacesFailure(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	calls := 0
	s, err := New(testConfig(t, clock), Dependencies{Maintenance: func(ctx context.Context) error { calls++; return errors.New("retention corrupt") }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.RunOnce(context.Background())
	if err != nil || calls != 1 || report.MaintenanceError != "retention corrupt" || s.Health().State != HealthDegraded || s.Health().LastMaintenanceError != "retention corrupt" {
		t.Fatalf("report=%+v calls=%d err=%v health=%+v", report, calls, err, s.Health())
	}
	s.deps.Maintenance = func(context.Context) error { return nil }
	report, err = s.RunOnce(context.Background())
	if err != nil || report.MaintenanceError != "" || s.Health().State != HealthHealthy || s.Health().LastMaintenanceError != "" {
		t.Fatalf("report=%+v err=%v health=%+v", report, err, s.Health())
	}
}

func TestMaintenanceTimeoutRemainsVisibleInHealth(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	s, err := New(testConfig(t, clock), Dependencies{Maintenance: func(context.Context) error { return context.DeadlineExceeded }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		report, err := s.RunOnce(context.Background())
		if err != nil || report.MaintenanceError != "context deadline exceeded" || s.Health().State != HealthDegraded || s.Health().LastMaintenanceError != "context deadline exceeded" {
			t.Fatalf("report=%+v health=%+v err=%v", report, s.Health(), err)
		}
	}
}

func TestMaintenanceErrorTimestampDoesNotRefreshOnSkippedPass(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	at := clock.now
	failure := &MaintenanceFailure{Err: context.DeadlineExceeded, OccurredAt: at}
	s, err := New(testConfig(t, clock), Dependencies{Maintenance: func(context.Context) error { return failure }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		report, err := s.RunOnce(context.Background())
		if err != nil || !report.MaintenanceErrorAt.Equal(at) || !s.Health().LastMaintenanceErrorAt.Equal(at) {
			t.Fatalf("timestamp changed: report=%+v health=%+v %v", report, s.Health(), err)
		}
		clock.now = clock.now.Add(time.Minute)
	}
}
