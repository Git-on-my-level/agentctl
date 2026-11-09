package main

import (
	"testing"
	"time"
)

func TestAwaitPollBackoffResetsAndRespectsDeadline(t *testing.T) {
	var interval time.Duration
	for i, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		interval = nextAwaitPoll(interval, false)
		if interval != want {
			t.Fatalf("step %d: %s", i, interval)
		}
		pause := awaitPollPause(interval, 0, false)
		if pause < interval/2 || pause > interval {
			t.Fatalf("jitter=%s", pause)
		}
	}
	if nextAwaitPoll(interval, true) != 500*time.Millisecond {
		t.Fatal("progress did not reset")
	}
	if awaitPollPause(interval, 10*time.Millisecond, true) > 10*time.Millisecond {
		t.Fatal("overslept deadline")
	}
}
