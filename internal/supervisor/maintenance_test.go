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
	if err != nil || calls != 1 || report.MaintenanceError != "retention corrupt" || s.Health().State != HealthDegraded {
		t.Fatalf("report=%+v calls=%d err=%v health=%+v", report, calls, err, s.Health())
	}
	s.deps.Maintenance = func(context.Context) error { return nil }
	report, err = s.RunOnce(context.Background())
	if err != nil || report.MaintenanceError != "" || s.Health().State != HealthHealthy {
		t.Fatalf("report=%+v err=%v health=%+v", report, err, s.Health())
	}
}
