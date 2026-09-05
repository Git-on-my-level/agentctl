package fanout

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunSerialAdmissionOrder(t *testing.T) {
	var order []int
	attempted, err := Run(context.Background(), 8, 1, false, func(_ context.Context, i int) bool {
		order = append(order, i)
		return i != 3
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []int{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("order=%v", order)
	}
	for i, yes := range attempted {
		if !yes {
			t.Fatalf("child %d unexpectedly skipped", i)
		}
	}
}

func TestRunRespectsConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started, release := make(chan int, 8), make(chan struct{})
	done := make(chan error, 1)
	var active, high atomic.Int32
	go func() {
		_, err := Run(ctx, 8, 3, false, func(ctx context.Context, i int) bool {
			n := active.Add(1)
			defer active.Add(-1)
			for old := high.Load(); n > old && !high.CompareAndSwap(old, n); old = high.Load() {
			}
			started <- i
			select {
			case <-release:
				return true
			case <-ctx.Done():
				return false
			}
		})
		done <- err
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("callbacks did not start")
		}
	}
	select {
	case i := <-started:
		t.Fatalf("child %d exceeded concurrency", i)
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("did not drain")
	}
	if high.Load() != 3 || active.Load() != 0 {
		t.Fatalf("peak=%d active=%d", high.Load(), active.Load())
	}
}

func TestRunFailFastCancelsAndDrainsAdmittedSiblings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	secondStarted := make(chan struct{})
	var cleaned atomic.Bool
	attempted, err := Run(ctx, 8, 2, true, func(ctx context.Context, i int) bool {
		switch i {
		case 0:
			select {
			case <-secondStarted:
			case <-ctx.Done():
			}
			return false
		case 1:
			close(secondStarted)
			<-ctx.Done()
			cleaned.Store(true)
			return false
		default:
			t.Errorf("queued child %d launched after failure", i)
			return true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(attempted, []bool{true, true, false, false, false, false, false, false}) || !cleaned.Load() {
		t.Fatalf("attempted=%v cleaned=%v", attempted, cleaned.Load())
	}
}

func TestRunAlreadyCancelledStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempted, err := Run(ctx, 3, 2, false, func(context.Context, int) bool { t.Error("called after cancellation"); return true })
	if err != nil || !reflect.DeepEqual(attempted, []bool{false, false, false}) {
		t.Fatalf("attempted=%v err=%v", attempted, err)
	}
}

func TestRunExternalCancellationDrains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 2)
	done := make(chan []bool, 1)
	var cleaned atomic.Int32
	go func() {
		attempted, _ := Run(ctx, 6, 2, false, func(ctx context.Context, _ int) bool {
			started <- struct{}{}
			<-ctx.Done()
			cleaned.Add(1)
			return false
		})
		done <- attempted
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("not started")
		}
	}
	cancel()
	select {
	case attempted := <-done:
		if cleaned.Load() != 2 || !reflect.DeepEqual(attempted, []bool{true, true, false, false, false, false}) {
			t.Fatalf("attempted=%v cleaned=%d", attempted, cleaned.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not drained")
	}
}

func TestRunRejectsInvalidBounds(t *testing.T) {
	for _, limits := range [][2]int{{0, 1}, {65, 1}, {1, 0}, {1, 17}} {
		if _, err := Run(context.Background(), limits[0], limits[1], false, func(context.Context, int) bool { return true }); err == nil {
			t.Fatalf("accepted %v", limits)
		}
	}
	if _, err := Run(context.Background(), 1, 1, false, nil); err == nil {
		t.Fatal("accepted nil callback")
	}
}
