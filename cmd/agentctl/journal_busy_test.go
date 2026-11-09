package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func holdJournal(t *testing.T) (string, *store.Journal) {
	t.Helper()
	path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	holder, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	return path, holder
}

func TestJournalBusyBudgetWaitsForRelease(t *testing.T) {
	path, holder := holdJournal(t)
	released := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = holder.Close()
		close(released)
	}()
	start := time.Now()
	journal, err := openJournalWithBusyBudget(context.Background(), path, store.Options{}, false, 10*time.Second)
	if err != nil {
		t.Fatalf("open under contention: %v", err)
	}
	defer journal.Close()
	<-released
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("open returned before the holder released: %s", elapsed)
	}
}

func TestJournalBusyBudgetContextBoundWaitsForRelease(t *testing.T) {
	path, holder := holdJournal(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = holder.Close()
	}()
	scoped := scopedJournal{path: path, busyBudget: 10 * time.Second}
	if _, err := scoped.HostID(context.Background()); err != nil {
		t.Fatalf("scoped journal did not absorb contention: %v", err)
	}
}

func TestJournalBusyBudgetExhaustionIsTypedJournalBusy(t *testing.T) {
	path, _ := holdJournal(t)
	start := time.Now()
	_, err := openJournalWithBusyBudget(context.Background(), path, store.Options{}, false, 400*time.Millisecond)
	if !errors.Is(err, store.ErrBusy) {
		t.Fatalf("error=%v want ErrBusy", err)
	}
	var busy *journalBusyError
	if !errors.As(err, &busy) || busy.attempts < 1 || busy.budget != 400*time.Millisecond || busy.waited < 400*time.Millisecond {
		t.Fatalf("missing retry accounting: %#v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("budget overran: %s", elapsed)
	}
	problem := mapStoreError("open journal", err)
	if problem.ExitCode != 6 || problem.Details["diagnostic_code"] != "journal_busy" || problem.Details["busy_attempts"] == nil || problem.Details["busy_budget_ms"] != int64(400) {
		t.Fatalf("problem=%#v", problem)
	}
}

func TestJournalBusyBudgetHonorsCancellation(t *testing.T) {
	path, _ := holdJournal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := openJournalWithBusyBudget(ctx, path, store.Options{}, true, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v want deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation ignored for %s", elapsed)
	}
}

func TestJournalBusyBudgetZeroKeepsTwoAttempts(t *testing.T) {
	path, _ := holdJournal(t)
	_, err := openJournalWithBusyBudget(context.Background(), path, store.Options{LockTimeout: 20 * time.Millisecond}, false, 0)
	if !errors.Is(err, store.ErrBusy) {
		t.Fatalf("error=%v", err)
	}
	var busy *journalBusyError
	if errors.As(err, &busy) {
		t.Fatal("zero budget used the bounded retry loop")
	}
}

func TestResolveJournalBusyBudget(t *testing.T) {
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == journalBusyBudgetEnv {
				return value
			}
			return ""
		}
	}
	for _, test := range []struct {
		command, flag string
		flagSet       bool
		env           string
		want          time.Duration
		wantErr       bool
	}{
		{command: "delegate", want: defaultJournalBusyBudget},
		{command: "run", want: defaultJournalBusyBudget},
		{command: "dispatch", want: defaultJournalBusyBudget},
		{command: "continue", want: defaultJournalBusyBudget},
		{command: "await", want: defaultJournalBusyBudget},
		{command: "await-any", want: defaultJournalBusyBudget},
		{command: "result", want: defaultJournalBusyBudget},
		{command: "reconcile", want: defaultJournalBusyBudget},
		{command: "steer", want: defaultJournalBusyBudget},
		{command: "status", want: 0},
		{command: "status", env: "45s", want: 45 * time.Second},
		{command: "delegate", env: "0", want: 0},
		{command: "delegate", env: "30s", flag: "5s", flagSet: true, want: 5 * time.Second},
		{command: "delegate", flag: "-1s", flagSet: true, wantErr: true},
		{command: "delegate", flag: "31m", flagSet: true, wantErr: true},
		{command: "delegate", env: "soon", wantErr: true},
	} {
		got, err := resolveJournalBusyBudget(test.command, test.flag, test.flagSet, env(test.env))
		if (err != nil) != test.wantErr || (!test.wantErr && got != test.want) {
			t.Fatalf("%+v: got=%s err=%v", test, got, err)
		}
	}
}

func TestJournalBusyBudgetFlagValidatedBeforeCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal-busy-budget", "soon", "status", "exec-purple-monkey-dragon-river-candle-meadow"}); code != output.ExitCodeFor(output.CodeUsage) {
		t.Fatalf("exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "journal busy budget") {
		t.Fatalf("output=%s", stdout.String())
	}
}

func TestMalformedJournalBusyBudgetKeepsDiscoveryWorking(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.getenv = func(key string) string {
		if key == journalBusyBudgetEnv {
			return "soon"
		}
		return ""
	}
	if code := a.run(context.Background(), []string{"version"}); code != 0 {
		t.Fatalf("version exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"recent"}); code != output.ExitCodeFor(output.CodeUsage) || !strings.Contains(stdout.String(), journalBusyBudgetEnv) {
		t.Fatalf("recent exit=%d output=%s", code, stdout.String())
	}
}

// After the final backoff consumes the budget no further open is attempted,
// so a writer releasing just after the budget cannot turn exhaustion into a
// late success (or a further full lock wait).
func TestJournalBusyBudgetDoesNotOpenAfterBudget(t *testing.T) {
	path, holder := holdJournal(t)
	const budget = 150 * time.Millisecond
	original := journalBusySleep
	t.Cleanup(func() { journalBusySleep = original })
	start := time.Now()
	journalBusySleep = func(ctx context.Context, delay time.Duration) error {
		time.Sleep(delay)
		if time.Since(start) >= budget {
			_ = holder.Close()
		}
		return nil
	}
	journal, err := openJournalWithBusyBudget(context.Background(), path, store.Options{}, false, budget)
	if err == nil {
		_ = journal.Close()
		t.Fatal("open succeeded after the busy budget elapsed")
	}
	if !errors.Is(err, store.ErrBusy) {
		t.Fatalf("error=%v want ErrBusy", err)
	}
}

func TestAcknowledgementsAbsorbContentionAndHonorCancellation(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(fmt.Sprint("bulk=", bulk), func(t *testing.T) {
			path, holder := holdJournal(t)
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			execution := mustCreateTerminal(t, holder, "codex", model.StateCompleted, a.now().Add(-time.Hour))
			released := make(chan struct{})
			go func() { time.Sleep(250 * time.Millisecond); holder.Close(); close(released) }()
			c := common{journalPath: path, journalBusyBudget: journalBusyCommandDefault("result")}
			var problem *output.Error
			if bulk {
				_, problem = a.acknowledgeExecutions(context.Background(), c, []ids.ExecutionID{execution.ID}, store.AcknowledgementResult)
			} else {
				problem = a.acknowledgeExecution(context.Background(), c, execution.ID, store.AcknowledgementResult)
			}
			<-released
			if problem != nil {
				t.Fatal(problem)
			}
			j, err := store.Open(path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			if _, err := j.GetAcknowledgement(context.Background(), execution.ID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if bulk {
				_, problem = a.acknowledgeExecutions(ctx, c, []ids.ExecutionID{execution.ID}, store.AcknowledgementResult)
			} else {
				problem = a.acknowledgeExecution(ctx, c, execution.ID, store.AcknowledgementResult)
			}
			if problem == nil {
				t.Fatal("ack ignored cancelled collection context")
			}
		})
	}
}

func TestCleanupCommandKeepsLegacyOpenBudgetBetweenPages(t *testing.T) {
	path, holder := holdJournal(t)
	released := make(chan struct{})
	go func() { defer close(released); time.Sleep(700 * time.Millisecond); holder.Close() }()
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code := a.run(ctx, []string{"--journal", path, "data", "cleanup", "--before", time.Now().UTC().Format(time.RFC3339), "--plan"})
	<-released
	if code != 0 {
		t.Fatalf("cleanup's paged reader used a short lock poll instead of its command budget: exit=%d %s %s", code, out.String(), stderr.String())
	}
}

func TestCleanupZeroBudgetCancellationInterruptsHeldLock(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			path, holder := holdJournal(t)
			defer holder.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timer := time.AfterFunc(100*time.Millisecond, cancel)
			defer timer.Stop()
			var out, stderr bytes.Buffer
			a := testApp(&out, &stderr)
			args := []string{"--journal", path}
			if explicit {
				args = append(args, "--journal-busy-budget", "0")
			}
			args = append(args, "data", "cleanup", "--before", time.Now().UTC().Format(time.RFC3339), "--plan")
			start := time.Now()
			code := a.run(ctx, args)
			if code == 0 || time.Since(start) > time.Second {
				t.Fatalf("cancel stalled: exit=%d elapsed=%v %s", code, time.Since(start), stderr.String())
			}
		})
	}
}
