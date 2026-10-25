package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/store"
)

// Journal lock contention is agentctl's own resource, so commands that create
// or wait for work absorb it instead of handing every caller a retry loop.
// See docs/design/lane-fan-in.md.
const (
	defaultJournalBusyBudget = 120 * time.Second
	maxJournalBusyBudget     = 30 * time.Minute
	journalBusyBudgetEnv     = "AGENTCTL_JOURNAL_BUSY_BUDGET"
	journalBusyInitialDelay  = 100 * time.Millisecond
	journalBusyMaxDelay      = 5 * time.Second
	// Each attempt holds bbolt's own lock poll for at most this long so that
	// contenders interleave through the jittered sleep instead of polling in
	// lockstep for the whole budget.
	journalBusyAttemptLock = 2 * time.Second
)

// journalBusyCommands default to the bounded retry budget. Other commands
// keep the historical two-attempt open unless the operator sets a budget.
func journalBusyCommandDefault(command string) time.Duration {
	switch command {
	case "run", "delegate", "dispatch", "continue", "await", "await-any":
		return defaultJournalBusyBudget
	default:
		return 0
	}
}

func parseJournalBusyBudget(value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("journal busy budget must be a non-negative Go duration")
	}
	if parsed > maxJournalBusyBudget {
		return 0, fmt.Errorf("journal busy budget cannot exceed %s", maxJournalBusyBudget)
	}
	return parsed, nil
}

// resolveJournalBusyBudget applies flag, then environment, then the command
// default. An explicit 0 restores the historical two-attempt behavior.
func resolveJournalBusyBudget(command string, flagValue string, flagSet bool, getenv func(string) string) (time.Duration, error) {
	if flagSet {
		return parseJournalBusyBudget(flagValue)
	}
	if getenv != nil {
		if value := strings.TrimSpace(getenv(journalBusyBudgetEnv)); value != "" {
			budget, err := parseJournalBusyBudget(value)
			if err != nil {
				return 0, fmt.Errorf("%s: %w", journalBusyBudgetEnv, err)
			}
			return budget, nil
		}
	}
	return journalBusyCommandDefault(command), nil
}

// journalBusyError reports an exhausted contention budget. It wraps
// store.ErrBusy so existing journal_busy classification is unchanged.
type journalBusyError struct {
	attempts int
	waited   time.Duration
	budget   time.Duration
	err      error
}

func (e *journalBusyError) Error() string {
	return fmt.Sprintf("journal busy after %d attempts over %s (budget %s): %v", e.attempts, e.waited.Round(time.Millisecond), e.budget, e.err)
}
func (e *journalBusyError) Unwrap() error { return e.err }

// journalBusySleep is replaced by tests that need deterministic pacing.
var journalBusySleep = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func journalBusyJitter(delay time.Duration) time.Duration {
	if delay <= 1 {
		return delay
	}
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}

// openJournalWithBusyBudget retries lock contention with jittered exponential
// backoff until budget elapses. Any other open error returns immediately, and
// the caller's context deadline or cancellation always wins.
func openJournalWithBusyBudget(ctx context.Context, path string, options store.Options, contextBound bool, budget time.Duration) (*store.Journal, error) {
	if budget <= 0 {
		return openJournalWithRetryLoop(ctx, path, options, contextBound)
	}
	start := time.Now()
	deadline := start.Add(budget)
	delay := journalBusyInitialDelay
	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		if attempt > 1 && remaining <= 0 {
			// The last backoff consumed the budget; never open after it.
			return nil, &journalBusyError{attempts: attempt - 1, waited: time.Since(start), budget: budget, err: lastErr}
		}
		if ctxDeadline, ok := ctx.Deadline(); ok {
			if until := time.Until(ctxDeadline); until <= 0 {
				return nil, context.DeadlineExceeded
			} else if until < remaining {
				remaining = until
			}
		}
		lockTimeout := journalBusyAttemptLock
		if contextBound {
			lockTimeout = 50 * time.Millisecond
		}
		if options.LockTimeout > 0 && options.LockTimeout < lockTimeout {
			lockTimeout = options.LockTimeout
		}
		if remaining > 0 && remaining < lockTimeout {
			lockTimeout = remaining
		}
		if lockTimeout < time.Millisecond {
			// bbolt treats a zero timeout as "wait forever".
			lockTimeout = time.Millisecond
		}
		openOptions := options
		openOptions.LockTimeout = lockTimeout
		journal, err := store.Open(path, openOptions)
		if err == nil {
			if ctx.Err() != nil {
				_ = journal.Close()
				return nil, ctx.Err()
			}
			return journal, nil
		}
		if !errors.Is(err, store.ErrBusy) {
			return nil, err
		}
		lastErr = err
		if err := ctx.Err(); err != nil {
			// Cancellation or the caller's deadline wins over "busy".
			return nil, err
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return nil, &journalBusyError{attempts: attempt, waited: time.Since(start), budget: budget, err: lastErr}
		}
		sleep := journalBusyJitter(delay)
		if sleep > remaining {
			sleep = remaining
		}
		if err := journalBusySleep(ctx, sleep); err != nil {
			return nil, err
		}
		delay *= 2
		if delay > journalBusyMaxDelay {
			delay = journalBusyMaxDelay
		}
	}
}
