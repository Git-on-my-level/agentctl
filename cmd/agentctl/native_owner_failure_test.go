package main

import (
	"context"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

// Embed the unused methods so any call other than Wait fails the test.
type stuckOwnerRuntime struct {
	adapter.Adapter
	release <-chan struct{}
	started chan context.Context
	done    chan struct{}
}

func (r stuckOwnerRuntime) Wait(ctx context.Context, _ adapter.SourceRef) (adapter.Result, error) {
	defer close(r.done)
	r.started <- ctx
	<-r.release // Simulate an adapter that never produces a result, even on cancellation.
	return adapter.Result{State: adapter.StateCancelled}, nil
}

func TestSettleNativeOwnerErrorBoundsStuckWait(t *testing.T) {
	release := make(chan struct{})
	runtime := stuckOwnerRuntime{release: release, started: make(chan context.Context, 1), done: make(chan struct{})}
	defer func() { close(release); <-runtime.done }()
	problem := output.NewError(output.CodeExecutionUnknown, "owner failed", false)
	finished := make(chan *output.Error, 1)
	start := time.Now()
	go func() {
		finished <- (&app{}).settleNativeOwnerError(common{}, runtime, adapter.SourceRef{}, ids.ExecutionID(""), false, problem, nil)
	}()
	ctx := <-runtime.started
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Sub(start) > nativeOwnerSettleTimeout+time.Second {
		t.Errorf("Wait context deadline=%v present=%t", deadline, ok)
	}
	select {
	case got := <-finished:
		if got != problem || got.Details["cleanup"] != "result_unavailable" || got.Details["terminal_recorded"] != false || got.Details["child_reaped"] != nil {
			t.Fatalf("unexpected cleanup result: %+v", got)
		}
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("Wait returned before deadline: %v", ctx.Err())
		}
	case <-time.After(nativeOwnerSettleTimeout + 2*time.Second):
		t.Fatal("owner cleanup did not return within its deadline")
	}
}
