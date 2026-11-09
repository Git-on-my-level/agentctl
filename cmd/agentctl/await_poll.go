package main

import "time"

// Back off unchanged cached polls; progress resets the delay. Jitter prevents
// many waiters from repeatedly arriving in the same writer release window.
func nextAwaitPoll(previous time.Duration, changed bool) time.Duration {
	if changed || previous == 0 {
		return 500 * time.Millisecond
	}
	return min(previous*2, 4*time.Second)
}

func awaitPollPause(interval, remaining time.Duration, bounded bool) time.Duration {
	pause := journalBusyJitter(interval)
	if bounded {
		pause = min(pause, max(remaining, 0))
	}
	return pause
}
