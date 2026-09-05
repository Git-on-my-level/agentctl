package fanout

import (
	"context"
	"errors"
)

// Run admits callbacks in manifest order, with at most concurrency in flight.
// Actual process start and completion order remain concurrent. Failed callbacks
// cancel their siblings only when failFast is set. Callbacks must honor ctx and
// reap their own resources before returning; Run drains all admitted callbacks.
// The returned booleans distinguish attempted callbacks from unlaunched work.
// This is a bounded foreground loop, not a durable scheduler or a retry queue.
func Run(ctx context.Context, count, concurrency int, failFast bool, work func(context.Context, int) bool) ([]bool, error) {
	if count < 1 || count > MaxChildren || concurrency < 1 || concurrency > MaxConcurrency || work == nil {
		return nil, errors.New("invalid fanout execution bounds or callback")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	attempted := make([]bool, count)
	done := make(chan struct{}, concurrency)
	next, running := 0, 0
	for {
		for next < count && running < concurrency && runCtx.Err() == nil {
			i := next
			next++
			running++
			attempted[i] = true
			go func() {
				if !work(runCtx, i) && failFast {
					// Cancel before freeing the slot so a known failure cannot
					// race a successful sibling into admitting another job.
					cancel()
				}
				done <- struct{}{}
			}()
		}
		if running == 0 {
			return attempted, nil
		}
		<-done
		running--
	}
}
