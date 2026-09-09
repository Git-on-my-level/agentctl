package adapter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCursorThinkingCompletionCannotTurnDeadlineIntoSuccess(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"thinking","subtype":"completed"}'; exec sleep 30`)
	a := NewCursor()
	owner, stop := context.WithCancel(context.Background())
	defer stop()
	launch, err := a.Launch(owner, LaunchRequest{Argv: []string{path}, Timeout: 10 * time.Second, DiscoveryWindow: 50 * time.Millisecond, StartOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = a.Cancel(context.Background(), CancelRequest{Ref: launch.Session.Ref, Signal: "kill"})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = a.Wait(ctx, launch.Session.Ref)
	}()
	// Start the short wait deadline only once the fixture's phase was observed.
	// A launch-time deadline races process startup under race instrumentation.
	readyBy := time.Now().Add(5 * time.Second)
	var phase Event
	for phase.SourceState == "" {
		events, err := a.Events(context.Background(), EventsRequest{Ref: launch.Session.Ref})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.SourceState == "thinking" {
				phase = event
				break
			}
		}
		if phase.SourceState != "" {
			break
		}
		if time.Now().After(readyBy) {
			t.Fatal("fixture did not emit thinking completion")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = a.Wait(ctx, launch.Session.Ref)
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrTimeout {
		t.Fatalf("expected native wait timeout, got %v", err)
	}
	result, err := a.Result(context.Background(), ResultRequest{Ref: launch.Session.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.State != StateCancelled || result.Content != "" {
		t.Fatalf("unfinished thinking became task success: %#v", result)
	}
	if phase.Kind == "terminal" || phase.State != StateRunning {
		t.Fatalf("phase emitted terminal event: %#v", phase)
	}
}
