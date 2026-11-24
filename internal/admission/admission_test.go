package admission

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testBucket = "morph"

var testOwner = Owner{PID: 1, Start: "generation"}

func testConfig() Bucket {
	return Bucket{Harness: "opencode", Model: "morph/flash", MinWindow: 1, InitialWindow: 2, MaxWindow: 4, IncreaseIntervalSeconds: 1, SampleWindowSeconds: 3}
}

func TestControllerAIMDRequiresSamplesAndDemand(t *testing.T) {
	b := testConfig()
	start := time.Unix(100, 0).UTC()
	now := start
	s := controller{Window: 2, LastTick: now}
	tick := func(inUse, want int, why string) {
		t.Helper()
		now = now.Add(time.Second)
		s.expire(now, b)
		adjustment := s.tick(now, b, inUse)
		if s.Window != want || (adjustment == nil) != (why == "") || adjustment != nil && adjustment.Reason != why {
			t.Fatalf("window=%d adjustment=%#v, want %d %q", s.Window, adjustment, want, why)
		}
	}
	tick(2, 2, "") // Zero samples.
	s.observe(Sample{At: now, Attempts: 100, Throttled: 8})
	tick(2, 2, "") // Exactly at target.
	tick(2, 2, "")
	tick(2, 2, "") // Sample aged out.
	s.observe(Sample{At: now, Attempts: 100})
	tick(1, 2, "") // Spare capacity.
	s.tick(now.Add(500*time.Millisecond), b, 2)
	tick(1, 3, "below_target_with_demand") // Demand earlier in this interval.
	if again := s.tick(now.Add(time.Millisecond), b, 3); again != nil {
		t.Fatal(again)
	}
	s.observe(Sample{At: now, Attempts: 100})
	tick(3, 4, "below_target_with_demand")
	tick(4, 4, "") // Ceiling.
	s.observe(Sample{At: now, Attempts: 100, Throttled: 100})
	tick(4, 2, "above_target")
	tick(2, 1, "above_target")
	tick(1, 1, "") // Floor.
}

func TestObservationKeySurvivesReopenAndSampleExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	observed := now
	path := filepath.Join(t.TempDir(), "journal.db")
	open := func() *Store { return New(path, func() time.Time { return now }, nil) }
	b := testConfig()
	req := ObservationRequest{At: &observed, Attempts: 11, Throttled: 2, IdempotencyKey: "provider-batch-001"}
	if result, err := open().ObserveWithKey(ctx, testBucket, b, req); err != nil || !result.Recorded || result.Duplicate {
		t.Fatalf("first observation: %#v %v", result, err)
	}
	now = now.Add(b.lookback())
	if result, err := open().ObserveWithKey(ctx, testBucket, b, req); err != nil || result.Recorded || !result.Duplicate || result.Adjustment != nil {
		t.Fatalf("replay after sample expiry: %#v %v", result, err)
	}
	changed := req
	changed.Attempts++
	if _, err := open().ObserveWithKey(ctx, testBucket, b, changed); !errors.Is(err, ErrObservationKeyConflict) {
		t.Fatalf("changed observation: %v", err)
	}
	status, err := open().Status(ctx, testBucket, b)
	if err != nil || status.TotalAttempts != 11 || status.ThrottledAttempts != 2 || status.Attempts != 0 {
		t.Fatalf("replay changed counts: %#v %v", status, err)
	}
	data, err := os.ReadFile(filepath.Join(open().dir, testBucket, "controller.json"))
	if err != nil || bytes.Contains(data, []byte(req.IdempotencyKey)) {
		t.Fatalf("raw observation key persisted: %v", err)
	}
}

func TestObservationKeyReplayAfterBucketDisabled(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	observed := now
	path := filepath.Join(t.TempDir(), "journal.db")
	s := New(path, func() time.Time { return now }, nil)
	b := testConfig()
	req := ObservationRequest{At: &observed, Attempts: 7, Throttled: 2, IdempotencyKey: "provider-batch"}
	if result, err := s.ObserveWithKey(ctx, testBucket, b, req); err != nil || !result.Recorded {
		t.Fatalf("first observation: %#v %v", result, err)
	}
	controllerPath := filepath.Join(s.dir, testBucket, "controller.json")
	before, err := os.ReadFile(controllerPath)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	b.Enabled = &disabled
	now = now.Add(b.lookback())
	if result, err := s.ObserveWithKey(ctx, testBucket, b, req); err != nil || result.Recorded || !result.Duplicate || result.Adjustment != nil {
		t.Fatalf("disabled replay after sample expiry: %#v %v", result, err)
	}
	changed := req
	changed.Attempts++
	if _, err := s.ObserveWithKey(ctx, testBucket, b, changed); !errors.Is(err, ErrObservationKeyConflict) {
		t.Fatalf("disabled conflicting replay: %v", err)
	}
	fresh := ObservationRequest{At: &now, Attempts: 1, IdempotencyKey: "new-key"}
	if result, err := s.ObserveWithKey(ctx, testBucket, b, fresh); err != nil || result.Recorded || result.Duplicate {
		t.Fatalf("new disabled observation: %#v %v", result, err)
	}
	if result, err := s.ObserveWithKey(ctx, "missing", b, fresh); err != nil || result.Recorded || result.Duplicate {
		t.Fatalf("new disabled bucket: %#v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled replay created bucket state: %v", err)
	}
	now = observed.Add(observationKeyTTL)
	if _, err := s.ObserveWithKey(ctx, testBucket, b, req); !errors.Is(err, ErrInvalidSample) {
		t.Fatalf("expired key should return to sample-age validation: %v", err)
	}
	if after, err := os.ReadFile(controllerPath); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("disabled observations rewrote the controller: %v", err)
	}
}

func TestObservationKeyConcurrentReplayCommitsOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "journal.db")
	b := testConfig()
	req := ObservationRequest{Attempts: 2, Throttled: 1, IdempotencyKey: "concurrent-batch"}
	var recorded atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			s := New(path, time.Now, nil)
			result, err := s.ObserveWithKey(ctx, testBucket, b, req)
			if err != nil || result.Recorded == result.Duplicate {
				t.Errorf("observation receipt: %#v %v", result, err)
			}
			if result.Recorded {
				recorded.Add(1)
			}
		})
	}
	wg.Wait()
	status, err := New(path, time.Now, nil).Status(ctx, testBucket, b)
	if err != nil || recorded.Load() != 1 || status.TotalAttempts != 2 || status.ThrottledAttempts != 1 {
		t.Fatalf("recorded %d times: %#v %v", recorded.Load(), status, err)
	}
}

func TestObservationKeyCapacityRetainsExistingKeysUntilTTL(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	b := testConfig()
	s := New(filepath.Join(t.TempDir(), "journal.db"), func() time.Time { return now }, nil)
	req := ObservationRequest{Attempts: 1, IdempotencyKey: "existing-batch"}
	if result, err := s.ObserveWithKey(ctx, testBucket, b, req); err != nil || !result.Recorded {
		t.Fatalf("first observation: %#v %v", result, err)
	}
	dir := filepath.Join(s.dir, testBucket)
	state, warning := readController(dir, now, b)
	if warning != "" {
		t.Fatal(warning)
	}
	receipt := state.ObservationKeys[observationDigest(req.IdempotencyKey)]
	for i := 1; i < maxObservationKeys; i++ {
		state.ObservationKeys[observationDigest(fmt.Sprint(i))] = receipt
	}
	if err := writeJSON(dir, "controller.json", state); err != nil {
		t.Fatal(err)
	}
	now = now.Add(observationKeyTTL - time.Nanosecond)
	distinct := req
	distinct.IdempotencyKey = "new-batch"
	if _, err := s.ObserveWithKey(ctx, testBucket, b, distinct); !errors.Is(err, ErrObservationKeysFull) {
		t.Fatalf("expected full retention: %v", err)
	}
	if result, err := s.ObserveWithKey(ctx, testBucket, b, req); err != nil || !result.Duplicate {
		t.Fatalf("retained replay at capacity: %#v %v", result, err)
	}
	now = now.Add(time.Nanosecond)
	if result, err := s.ObserveWithKey(ctx, testBucket, b, distinct); err != nil || !result.Recorded {
		t.Fatalf("expired receipts still occupy capacity: %#v %v", result, err)
	}
	status, err := s.Status(ctx, testBucket, b)
	if err != nil || status.TotalAttempts != 2 {
		t.Fatalf("unaccepted observations changed totals: %#v %v", status, err)
	}
}

func TestQueuePriorityFIFOAndFallback(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "journal.db"), time.Now, nil)
	b := testConfig()
	b.StaticFallbackWindow, b.InitialWindow = 1, 1
	newQueue := func(holder, priority string) *Queue {
		q := s.Queue(testBucket, b, holder, priority, testOwner, testOwner, false)
		t.Cleanup(q.Close)
		return q
	}
	try := func(q *Queue, granted bool, position int) Decision {
		t.Helper()
		d, err := q.Try(ctx, true)
		if err != nil || d.Granted != granted || d.Position != position {
			t.Fatalf("%s: %#v, %v", q.holder, d, err)
		}
		return d
	}
	first := newQueue("first", Background)
	if d := try(first, true, 0); d.Window != 1 || d.Warning != "admission_state_missing" {
		t.Fatal(d)
	}
	if err := os.WriteFile(filepath.Join(s.dir, testBucket, "controller.json"), []byte("partial JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	back := newQueue("back", Background)
	if d := try(back, false, 1); d.Window != 1 || d.Warning != "admission_state_invalid" {
		t.Fatal(d)
	}
	front := newQueue("front", Interactive)
	try(front, false, 1)
	try(back, false, 2)
	second := newQueue("second", Interactive)
	try(second, false, 2)
	first.Close()
	try(back, false, 3)
	try(front, true, 0)
	front.Close()
	try(second, true, 0)
	second.Close()
	try(back, true, 0)
}

func TestOverWindowLeaseCountsTowardsCapacityAndDemand(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := New(filepath.Join(t.TempDir(), "journal.db"), func() time.Time { return now }, nil)
	b := testConfig()
	b.InitialWindow, b.StaticFallbackWindow = 1, 1
	newQueue := func(holder, priority string) *Queue {
		q := s.Queue(testBucket, b, holder, priority, testOwner, Owner{}, false)
		t.Cleanup(q.Close)
		return q
	}
	active := newQueue("active", Background)
	if d, err := active.Try(ctx, true); err != nil || !d.Granted {
		t.Fatalf("grant: %#v %v", d, err)
	}
	timedOut := newQueue("timed-out", Background)
	if d, err := timedOut.Try(ctx, true); err != nil || d.Granted {
		t.Fatalf("queue: %#v %v", d, err)
	}
	if d, err := timedOut.TryAfterMaxWait(ctx); err != nil || !d.Granted || d.Reason != "over_window" {
		t.Fatalf("max wait: %#v %v", d, err)
	}
	status, err := s.Status(ctx, testBucket, b)
	if err != nil || status.InUse != 2 || status.OverWindow != 1 || status.Admitted != 2 ||
		status.Holders[0].OverWindow || !status.Holders[1].OverWindow || status.Queued[Background] != 0 {
		t.Fatalf("extra lease was not counted: %#v %v", status, err)
	}
	active.Close()
	next := newQueue("waiting", Interactive)
	if d, err := next.Try(ctx, true); err != nil || d.Granted {
		t.Fatalf("over-window holder did not use capacity: %#v %v", d, err)
	}
	now = now.Add(2 * b.interval())
	adjustment, _, err := s.Observe(ctx, testBucket, b, Sample{At: now, Attempts: 1})
	if err != nil || adjustment == nil || adjustment.To != 2 {
		t.Fatalf("over-window holder did not register as current demand: %#v %v", adjustment, err)
	}
	timedOut.Close()
	status, err = s.Status(ctx, testBucket, b)
	if err != nil || status.InUse != 0 || status.OverWindow != 0 {
		t.Fatalf("extra lease survived release: %#v %v", status, err)
	}
}

func TestOverWindowLeaseSurvivesControllerWriteFailure(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "journal.db"), time.Now, nil)
	b := testConfig()
	b.InitialWindow, b.StaticFallbackWindow = 1, 1
	active := s.Queue(testBucket, b, "active", Background, testOwner, Owner{}, false)
	defer active.Close()
	if d, err := active.Try(ctx, true); err != nil || !d.Granted {
		t.Fatalf("grant: %#v %v", d, err)
	}
	path := filepath.Join(s.dir, testBucket, "controller.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	q := s.Queue(testBucket, b, "timed-out", Background, testOwner, Owner{}, false)
	defer q.Close()
	if d, err := q.TryAfterMaxWait(ctx); err != nil || !d.Granted || d.Warning != "admission_state_write_failed" {
		t.Fatalf("max wait: %#v %v", d, err)
	}
	status, err := s.Status(ctx, testBucket, b)
	if err != nil || status.InUse != 2 || status.OverWindow != 1 {
		t.Fatalf("extra lease was not counted: %#v %v", status, err)
	}
}

func TestExternalLeaseOwnerIdentityAndExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	starts := map[int]string{1: "invoker", 42: "owner"}
	var unknown bool
	s := New(filepath.Join(t.TempDir(), "journal.db"), func() time.Time { return now }, func(pid int) (string, error) {
		if unknown {
			return "", errors.New("kernel unavailable")
		}
		return starts[pid], nil
	})
	b := testConfig()
	check := func(grant Owner, holder, reason string) {
		t.Helper()
		q := s.Queue(testBucket, b, holder, Background, Owner{1, "invoker"}, grant, true)
		defer q.Close()
		d, err := q.Try(ctx, false)
		if err != nil || !d.Granted || d.Reason != reason {
			t.Fatalf("decision %#v, error %v", d, err)
		}
	}
	check(Owner{42, "owner"}, "external", "slot")
	check(Owner{42, "owner"}, "external", "reused")
	status, err := s.Status(ctx, testBucket, b)
	if err != nil || status.InUse != 1 || status.Admitted != 1 {
		t.Fatalf("%#v %v", status, err)
	}
	starts[42] = "replacement"
	status, err = s.Status(ctx, testBucket, b)
	if err != nil || status.InUse != 0 {
		t.Fatalf("reused PID: %#v %v", status, err)
	}
	check(Owner{42, "replacement"}, "external", "slot")
	unknown = true
	now = now.Add(59 * time.Second)
	status, err = s.Status(ctx, testBucket, b)
	if err != nil || status.InUse != 1 {
		t.Fatalf("early expiry: %#v %v", status, err)
	}
	now = now.Add(time.Second)
	status, err = s.Status(ctx, testBucket, b)
	if err != nil || status.InUse != 0 {
		t.Fatalf("unverified owner expired: %#v %v", status, err)
	}
	if released, err := s.Release(ctx, testBucket, "external"); err != nil || !released {
		t.Fatalf("%v %v", released, err)
	}
	if released, err := s.Release(ctx, testBucket, "external"); err != nil || released {
		t.Fatalf("%v %v", released, err)
	}
}

func TestLeaseProcessHelper(t *testing.T) {
	path := os.Getenv("ADMISSION_TEST_HELPER")
	if path == "" {
		return
	}
	s := New(path, time.Now, nil)
	b := testConfig()
	b.StaticFallbackWindow, b.InitialWindow, b.MaxWindow = 1, 1, 1
	q := s.Queue(testBucket, b, "child", Background, Owner{os.Getpid(), "fixture"}, Owner{}, false)
	defer q.Close()
	d, err := q.Try(context.Background(), false)
	if err != nil || !d.Granted {
		t.Fatalf("%#v %v", d, err)
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

func TestProcessCrashRetainsUnboundSlotAcrossProcessesUntilExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "journal.db")
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLeaseProcessHelper$")
	cmd.Env = append(os.Environ(), "ADMISSION_TEST_HELPER="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	}()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child: %q %v", line, err)
	}
	now := time.Now()
	s := New(path, func() time.Time { return now }, func(int) (string, error) { return testOwner.Start, nil })
	b := testConfig()
	b.StaticFallbackWindow, b.InitialWindow, b.MaxWindow = 1, 1, 1
	q := s.Queue(testBucket, b, "external", Interactive, testOwner, testOwner, true)
	defer q.Close()
	if d, err := q.Try(ctx, false); err != nil || d.Granted {
		t.Fatalf("over-admitted: %#v %v", d, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if d, err := q.Try(ctx, false); err != nil || d.Granted {
		t.Fatalf("owner crash released unbound slot early: %#v %v", d, err)
	}
	now = now.Add(launchLeaseTTL)
	if d, err := q.Try(ctx, false); err != nil || !d.Granted {
		t.Fatalf("expired launch gap leaked slot: %#v %v", d, err)
	}
}

func TestNativeLeaseTracksChildGenerationAfterOwnerUnlock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	now := time.Now().UTC()
	b := testConfig()
	b.InitialWindow, b.StaticFallbackWindow, b.MaxWindow = 1, 1, 1
	start, inspectErr := "child-generation", error(nil)
	childStart := func(pid int) (string, error) {
		if pid != 42 {
			t.Fatalf("inspected pid %d instead of child", pid)
		}
		return start, inspectErr
	}
	open := func() *Store { return New(path, func() time.Time { return now }, childStart) }
	q := open().Queue(testBucket, b, "native", Background, testOwner, Owner{}, false)
	defer q.Close()
	if d, err := q.Try(ctx, false); err != nil || !d.Granted {
		t.Fatalf("grant: %#v %v", d, err)
	}
	now = now.Add(launchLeaseTTL)
	if err := q.PrepareLaunch(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := open().Status(ctx, testBucket, b)
	if err != nil || view.InUse != 1 || view.Holders[0].LaunchLeaseExpiresAt == nil ||
		!view.Holders[0].LaunchLeaseExpiresAt.Equal(now.Add(launchLeaseTTL)) {
		t.Fatalf("refreshed launch grace: %#v %v", view, err)
	}
	if err := q.BindChild(42); err != nil {
		t.Fatal(err)
	}
	q.Close()
	now = now.Add(2 * launchLeaseTTL)
	view, err = open().Status(ctx, testBucket, b)
	if err != nil || view.InUse != 1 || view.Holders[0].Child == nil ||
		*view.Holders[0].Child != (Owner{42, start}) || view.Holders[0].LaunchLeaseExpiresAt != nil {
		t.Fatalf("running child: %#v %v", view, err)
	}
	start, inspectErr = "", errors.New("process inspection unavailable")
	contender := open().Queue(testBucket, b, "next", Interactive, testOwner, Owner{}, false)
	defer contender.Close()
	if d, err := contender.Try(ctx, false); err != nil || d.Granted {
		t.Fatalf("uncertain child must remain counted: %#v %v", d, err)
	}
	start, inspectErr = "reused-child-pid", nil
	if d, err := contender.Try(ctx, false); err != nil || !d.Granted {
		t.Fatalf("old child generation still counted: %#v %v", d, err)
	}
}

func TestConcurrentOwnersNeverExceedWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := New(filepath.Join(t.TempDir(), "journal.db"), time.Now, nil)
	b := testConfig()
	b.MaxWindow = b.InitialWindow
	var wg sync.WaitGroup
	var active atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := s.Queue(testBucket, b, fmt.Sprintf("run-%d", i), Background, testOwner, testOwner, false)
			defer q.Close()
			for {
				d, err := q.Try(ctx, true)
				if errors.Is(err, ErrLockBusy) {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				if d.Granted {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if n := active.Add(1); n > int32(b.MaxWindow) {
				t.Errorf("granted %d concurrent slots", n)
			}
			time.Sleep(15 * time.Millisecond)
			active.Add(-1)
		}()
	}
	wg.Wait()
	status, err := s.Status(ctx, testBucket, b)
	if err != nil || status.InUse != 0 || status.Admitted != 16 {
		t.Fatalf("%#v %v", status, err)
	}
}

func TestKillSwitchAndReadOnlyStatusCreateNoState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	s := New(path, time.Now, nil)
	b := testConfig()
	disabled := false
	b.Enabled = &disabled
	cfg := &Config{Buckets: map[string]Bucket{testBucket: b}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if name, bucket := cfg.Match("opencode", "morph/flash"); name != testBucket || bucket.Active() {
		t.Fatal(name, bucket)
	}
	view, err := s.Status(ctx, testBucket, b)
	if err != nil || view.Enabled {
		t.Fatalf("%#v %v", view, err)
	}
	q := s.Queue(testBucket, b, "native", Interactive, testOwner, testOwner, false)
	defer q.Close()
	if d, err := q.Try(ctx, true); err != nil || !d.Granted || d.Reason != "disabled" {
		t.Fatalf("%#v %v", d, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected state: %v", err)
	}
}

func TestExternalWaiterReplayUsesSameTicket(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "journal.db"), time.Now, func(int) (string, error) { return "owner", nil })
	b := testConfig()
	b.InitialWindow, b.MaxWindow = 1, 1
	native := s.Queue(testBucket, b, "native", Background, testOwner, testOwner, false)
	defer native.Close()
	if d, err := native.Try(ctx, false); err != nil || !d.Granted {
		t.Fatalf("%#v %v", d, err)
	}
	owner := Owner{PID: 42, Start: "owner"}
	waiter := s.Queue(testBucket, b, "external", Background, testOwner, owner, true)
	defer waiter.Close()
	if d, err := waiter.Try(ctx, true); err != nil || d.Granted || d.Position != 1 {
		t.Fatalf("%#v %v", d, err)
	}
	replay := s.Queue(testBucket, b, "external", Interactive, Owner{PID: 2, Start: "another-cli"}, owner, true)
	defer replay.Close()
	if d, err := replay.Try(ctx, true); err != nil || d.Granted || d.Position != 1 ||
		d.Reason != "already_queued" || d.Priority != Background {
		t.Fatalf("%#v %v", d, err)
	}
	native.Close()
	if d, err := waiter.Try(ctx, true); err != nil || !d.Granted {
		t.Fatalf("%#v %v", d, err)
	}
	if d, err := replay.Try(ctx, true); err != nil || !d.Granted || d.Reason != "reused" {
		t.Fatalf("%#v %v", d, err)
	}
}

func TestControllerRetainsFreshSamplesAtLookbackBoundary(t *testing.T) {
	b := testConfig()
	cutoff := time.Unix(100, 650_000_000).UTC()
	old := cutoff.Add(-150 * time.Millisecond)
	newest := cutoff.Add(150 * time.Millisecond)
	s := controller{}
	// Out-of-order timestamps within one second keep the freshest point.
	s.observe(Sample{At: newest, Attempts: 2, Throttled: 1})
	s.observe(Sample{At: old, Attempts: 1, Throttled: 1})
	s.expire(cutoff.Add(b.lookback()), b)
	if a, tCount := s.counts(cutoff.Add(b.lookback())); a != 3 || tCount != 2 || len(s.Samples) != 1 {
		t.Fatalf("fresh throttle sample was lost: %#v", s.Samples)
	}
	s.expire(newest.Add(b.lookback()), b)
	if len(s.Samples) != 0 {
		t.Fatalf("expired sample retained: %#v", s.Samples)
	}
}

func TestQueueResumesAdaptiveWindowAfterLockTimeout(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "journal.db"), time.Now, nil)
	b := testConfig()
	b.StaticFallbackWindow = 1
	active := s.Queue(testBucket, b, "active", Background, testOwner, testOwner, false)
	defer active.Close()
	if d, err := active.Try(ctx, false); err != nil || !d.Granted {
		t.Fatalf("seed slot: %#v %v", d, err)
	}
	waiter := s.Queue(testBucket, b, "waiter", Background, testOwner, testOwner, false)
	defer waiter.Close()
	waiter.lockTimedOut = true
	if d, err := waiter.Try(ctx, true); err != nil || d.Granted || d.Window != 1 || d.Warning != "admission_lock_timeout" {
		t.Fatalf("first recovered poll: %#v %v", d, err)
	}
	if d, err := waiter.Try(ctx, true); err != nil || !d.Granted || d.Window != 2 {
		t.Fatalf("adaptive window did not recover: %#v %v", d, err)
	}
}
