package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestOpenCodeBackoff(t *testing.T) {
	for _, tt := range []struct {
		attempt    int
		retryAfter int64
		base, want time.Duration
	}{
		{1, 10_000, time.Minute, 90 * time.Second},
		{2, 10_000, time.Minute, 150 * time.Second},
		{3, 10_000, time.Minute, 270 * time.Second},
		{1, 100_000, 100 * time.Second, 150 * time.Second},
	} {
		got := openCodeBackoff(tt.attempt, tt.retryAfter, func(bound time.Duration) time.Duration {
			if bound != tt.base {
				t.Fatalf("jitter bound %s, want %s", bound, tt.base)
			}
			return bound / 2
		})
		if got != tt.want {
			t.Errorf("attempt %d retry-after %d: delay=%s want=%s", tt.attempt, tt.retryAfter, got, tt.want)
		}
	}
}

func TestOpenCodeRetryFloorPreservesFutureRetryTime(t *testing.T) {
	now := time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC)
	future := now.Add(120*time.Second + time.Nanosecond)
	retry := &model.NativeRateLimit{RetryAfterMS: 65_000, NextRetryAt: &future}
	if got := openCodeRetryFloorMS(90_000, retry, now); got != 120_001 {
		t.Fatalf("retry floor=%d, want 120001", got)
	}
	if got := openCodeRetryFloorMS(90_000, retry, future); got != 90_000 {
		t.Fatalf("retry floor after next_retry_at=%d, want 90000", got)
	}
}

func automaticOpenCodeFixture(t *testing.T, turns string) delegateFixture {
	t.Helper()
	f := carryFixture(t, "opencode")
	t.Setenv("MULTICA_TASK_ID", "")
	t.Setenv("TEST_OPENCODE_TURNS", turns)
	t.Setenv("TEST_OPENCODE_DIR", f.root)
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'opencode v2.3.2'; exit 0; fi
if [ "$1" = session ]; then
  [ "$#" = 5 ] && [ "$2" = export ] && [ "$3" = --sanitize ] && [ "$4" = -- ] && [ "$5" = ses_recover ] || exit 21
  [ "${TEST_OPENCODE_EXPORT_MISSING:-0}" = 0 ] || exit 22
  echo private-native-export
  exit 0
fi
launches="$TEST_OPENCODE_DIR/launches"
echo launch >> "$launches"
printf '%s\n' "$PPID" >> "$TEST_OPENCODE_DIR/owner-pids"
step=$(( $(wc -l < "$launches") + 0 ))
for arg do instruction="$arg"; done
case "$step" in
  1) [ "$instruction" = private-original-task ] || exit 23
     case " $* " in *" --session "*) exit 24;; esac ;;
  *) case " $* " in *" --session ses_recover -- "*) :;; *) exit 25;; esac
     prev=$((step - 1))
     reason=$(printf %s "$TEST_OPENCODE_TURNS" | cut -c "$prev")
     case "$reason" in
       r) [ "$instruction" = '` + openCodeRateLimitInstruction + `' ] || exit 26 ;;
       e) [ "$instruction" = '` + openCodeFinalAnswerInstruction + `' ] || exit 27 ;;
       *) [ "$instruction" = 'manual native follow-up' ] || exit 28 ;;
     esac ;;
esac
printf '%s\n' '{"type":"step_start","sessionID":"ses_recover","part":{"type":"step-start"}}'
case "$(printf %s "$TEST_OPENCODE_TURNS" | cut -c "$step")" in
  r)
    printf '%s\n' '{"type":"retry","sessionID":"ses_recover","attempt":4,"error":{"type":"provider.rate-limit","message":"private-provider-body","status":429,"retryAfterMs":65000}}'
    printf '%s\n' '{"type":"error","sessionID":"ses_recover","error":{"type":"provider.rate-limit","message":"private-provider-body","status":429,"retryAfterMs":90000}}'
    exit 1 ;;
  e) printf '%s\n' '{"type":"reasoning","sessionID":"ses_recover","part":{"type":"reasoning","text":"private-reasoning"}}' ;;
  *) printf '%s\n' '{"type":"text","sessionID":"ses_recover","part":{"type":"text","text":"recovered answer","time":{"end":1}}}' ;;
esac
printf '%s\n' '{"type":"step_finish","sessionID":"ses_recover","part":{"type":"step-finish","reason":"stop"}}'
`
	if err := os.WriteFile(filepath.Join(f.root, "cursor-agent"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

func disableOpenCodeRecovery(t *testing.T, f delegateFixture, rateLimit, finalAnswer bool) {
	t.Helper()
	cfg, err := config.Load(f.config)
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.Profiles[cfg.DefaultProfile]
	if profile.Delegation == nil {
		profile.Delegation = &config.DelegationPolicy{}
	}
	off := false
	if rateLimit {
		profile.Delegation.OpenCodeRateLimitRetry = &off
	}
	if finalAnswer {
		profile.Delegation.OpenCodeFinalAnswerFollowup = &off
	}
	cfg.Profiles[cfg.DefaultProfile] = profile
	if err := config.Save(f.config, cfg, true); err != nil {
		t.Fatal(err)
	}
}

func invokeAutomaticOpenCode(ctx context.Context, f delegateFixture, delay func(int, int64) time.Duration, extra ...string) (int, string) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("private-original-task")
	a.openCodeRecoveryDelay = delay
	args := []string{"--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin"}
	code := a.run(ctx, append(args, extra...))
	return code, stdout.String()
}

func openCodeChain(t *testing.T, text string) []ids.ExecutionID {
	t.Helper()
	var doc struct {
		Result struct {
			Chain []ids.ExecutionID `json:"recovery_chain"`
		} `json:"result"`
		Error struct {
			Details struct {
				Chain []ids.ExecutionID `json:"recovery_chain"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("decode chain: %v %s", err, text)
	}
	if len(doc.Result.Chain) != 0 {
		return doc.Result.Chain
	}
	return doc.Error.Details.Chain
}

func TestOpenCodeAutomaticRecoveryChain(t *testing.T) {
	for _, tt := range []struct {
		name, turns string
		launches    int
		exit        int
	}{
		{"mixed recovery", "rers", 4, 0},
		{"capacity cap", "rrrrs", 4, 11},
		{"final answer once", "ees", 2, 11},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := automaticOpenCodeFixture(t, tt.turns)
			var delays []int
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			code, out := invokeAutomaticOpenCode(ctx, f, func(attempt int, ms int64) time.Duration {
				delays = append(delays, attempt)
				if ms != 90_000 {
					t.Errorf("terminal retry-after=%d, want 90000", ms)
				}
				return 0
			}, "--wait")
			chain := openCodeChain(t, out)
			if code != tt.exit || len(chain) != tt.launches || f.launches(t) != tt.launches {
				t.Fatalf("exit=%d chain=%v launches=%d output=%s", code, chain, f.launches(t), out)
			}
			for i, n := range delays {
				if n != i+1 {
					t.Fatalf("attempts=%v", delays)
				}
			}
			root, last := chain[0].String(), chain[len(chain)-1].String()
			for _, command := range []string{"status", "await", "result"} {
				code, out = f.command(t, "", command, root)
				want := 0
				if command == "await" {
					want = tt.exit
				}
				if code != want || !slices.Equal(openCodeChain(t, out), chain) || !strings.Contains(out, root) {
					t.Fatalf("%s exit=%d: %s", command, code, out)
				}
				if tt.name == "capacity cap" && (!strings.Contains(out, failureMessageOpenCodeRateLimit) || !strings.Contains(out, `"http_status":429`)) {
					t.Fatalf("%s missing structured 429 diagnostic: %s", command, out)
				}
			}
			if tt.exit == 0 {
				for _, command := range []string{"await", "result"} {
					if code, out = f.command(t, "", command, root, "--content"); code != 0 || out != "recovered answer" {
						t.Fatalf("%s --content exit=%d: %q", command, code, out)
					}
				}
				code, out = f.invoke("private-original-task")
				if code != 0 || !strings.Contains(out, `"state":"completed"`) || !strings.Contains(out, `"reused":true`) || !slices.Equal(openCodeChain(t, out), chain) {
					t.Fatalf("receipt replay exit=%d: %s", code, out)
				}
			}
			code, out = f.invoke("private-original-task", "--wait")
			if code != tt.exit || !slices.Equal(openCodeChain(t, out), chain) || f.launches(t) != tt.launches {
				t.Fatalf("same-key replay exit=%d: %s", code, out)
			}
			if code, events := f.command(t, "", "events", root); code != 0 || !strings.Contains(events, `"relation":"opencode_recovery"`) || strings.Contains(events, last) != (len(chain) == 2) {
				t.Fatalf("root event exit=%d: %s", code, events)
			}
			j, err := store.Open(f.journal, store.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range chain {
				if _, err := j.GetAcknowledgement(ctx, id); err != nil {
					j.Close()
					t.Fatalf("chain member %s remains uncollected: %v", id, err)
				}
			}
			j.Close()
			journal, err := os.ReadFile(f.journal)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"private-original-task", "private-provider-body", "private-reasoning", "private-native-export", openCodeRateLimitInstruction, openCodeFinalAnswerInstruction} {
				if bytes.Contains(journal, []byte(private)) {
					t.Fatalf("private content entered journal: %q", private)
				}
			}
		})
	}
}

func TestOpenCodeRecoveredLogicalIDCanContinue(t *testing.T) {
	f := automaticOpenCodeFixture(t, "es")
	code, out := f.invoke("private-original-task", "--wait")
	chain := openCodeChain(t, out)
	if code != 0 || len(chain) != 2 {
		t.Fatalf("recovery exit=%d: %s", code, out)
	}
	root, physical := chain[0].String(), chain[1].String()
	code, out = f.command(t, "manual carry-forward", "continue", root, "--request-key", "carry", "--mode", "carry_forward", "--prompt-stdin", "--plan")
	if code != 0 || !strings.Contains(out, `"continues":"`+physical+`"`) || !strings.Contains(out, `"native_session_reused":false`) {
		t.Fatalf("carry-forward plan exit=%d: %s", code, out)
	}
	args := []string{"continue", root, "--request-key", "manual", "--prompt-stdin", "--wait"}
	code, out = f.command(t, "manual native follow-up", args...)
	if code != 0 || !strings.Contains(out, `"continues":"`+physical+`"`) || f.launches(t) != 3 {
		t.Fatalf("native follow-up exit=%d: %s", code, out)
	}
	successor := resultID(t, out)
	code, out = f.command(t, "manual native follow-up", args...)
	if code != 0 || resultID(t, out) != successor || !strings.Contains(out, `"reused":true`) || f.launches(t) != 3 {
		t.Fatalf("native follow-up replay exit=%d: %s", code, out)
	}
	code, out = f.command(t, "", "status", root)
	if code != 0 || !slices.Equal(openCodeChain(t, out), chain) {
		t.Fatalf("logical status followed manual turn: %s", out)
	}
}

func TestOpenCodeBulkResultCollectsOneLogicalExecution(t *testing.T) {
	for _, prior := range []string{"unacknowledged", "original_acknowledged", "final_acknowledged"} {
		t.Run(prior, func(t *testing.T) {
			f := automaticOpenCodeFixture(t, "es")
			code, out := f.invoke("private-original-task")
			chain := openCodeChain(t, out)
			if code != 0 || len(chain) != 2 {
				t.Fatalf("recovery exit=%d: %s", code, out)
			}
			want := 1
			switch prior {
			case "original_acknowledged":
				if code, out = f.command(t, "", "await-any", "--execution", chain[0].String()); code != 11 {
					t.Fatalf("physical await-any exit=%d: %s", code, out)
				}
			case "final_acknowledged":
				if code, out = f.command(t, "", "result", chain[1].String(), "--content"); code != 0 {
					t.Fatalf("physical result exit=%d: %s", code, out)
				}
				want = 0
			}
			code, out = f.command(t, "", "result", "--unreconciled")
			var doc struct {
				Result struct {
					Items []unreconciledResultItem `json:"executions"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 || len(doc.Result.Items) != want {
				t.Fatalf("bulk result exit=%d: %s", code, out)
			}
			if want == 0 {
				return
			}
			item := doc.Result.Items[0]
			if item.ExecutionID != chain[0].String() || !slices.Equal(item.RecoveryChain, chain) ||
				!item.Acknowledged || item.Outcome == nil || item.Outcome.Content == nil || item.Outcome.Content.Text != "recovered answer" {
				t.Fatalf("bulk result did not collect the logical chain: %s", out)
			}
		})
	}
}

func TestOpenCodeAutomaticRecoveryKillSwitchAndMissingSession(t *testing.T) {
	for _, tt := range []struct {
		name, turns  string
		rate, final  bool
		missing      bool
		exit         int
		wantSchedule bool
	}{
		{"capacity disabled", "rs", true, false, false, 10, false},
		{"final answer disabled", "es", false, true, false, 11, false},
		{"session export refused", "es", false, false, true, 11, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := automaticOpenCodeFixture(t, tt.turns)
			disableOpenCodeRecovery(t, f, tt.rate, tt.final)
			if tt.missing {
				t.Setenv("TEST_OPENCODE_EXPORT_MISSING", "1")
			}
			code, out := f.invoke("private-original-task", "--wait")
			chain := openCodeChain(t, out)
			if code != tt.exit || (len(chain) == 1) != tt.wantSchedule || f.launches(t) != 1 {
				t.Fatalf("exit=%d launches=%d output=%s", code, f.launches(t), out)
			}
		})
	}
}

func TestOpenCodeRecoveryPendingCanBeObservedAndCancelled(t *testing.T) {
	f := automaticOpenCodeFixture(t, "rs")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		code, _ := invokeAutomaticOpenCode(ctx, f, func(int, int64) time.Duration { return 10 * time.Second }, "--wait")
		done <- code
	}()
	var id string
	for ctx.Err() == nil && id == "" {
		journal, err := store.Open(f.journal, store.Options{ReadOnly: true, LockTimeout: 100 * time.Millisecond})
		if err == nil {
			rows, err := journal.ListExecutions(ctx, false)
			journal.Close()
			if err == nil && len(rows) == 1 && rows[0].OpenCodeRecoveryPending(time.Now()) {
				id = rows[0].ID.String()
			}
		}
		if id == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if id == "" {
		t.Fatal("pending recovery never became visible")
	}
	code, out := f.command(t, "", "status", id)
	if code != 0 || !strings.Contains(out, `"state":"waiting"`) || len(openCodeChain(t, out)) != 1 {
		t.Fatalf("pending status exit=%d: %s", code, out)
	}
	code, out = f.command(t, "", "await", id, "--timeout", "100ms")
	if code != 9 || !strings.Contains(out, "await deadline elapsed") || !strings.Contains(out, "recovery_chain") {
		t.Fatalf("pending await exit=%d: %s", code, out)
	}
	code, out = f.command(t, "", "result", id)
	if code != 8 || !strings.Contains(out, "execution is not terminal") || !strings.Contains(out, "recovery_chain") {
		t.Fatalf("pending result exit=%d: %s", code, out)
	}
	code, out = f.command(t, "", "result", "--unreconciled")
	if code != 0 || !strings.Contains(out, `"status":"skipped"`) || !strings.Contains(out, `"acknowledged":false`) {
		t.Fatalf("pending bulk result exit=%d: %s", code, out)
	}
	code, out = f.command(t, "", "cancel", id)
	if code != 0 || !strings.Contains(out, "opencode_recovery_cancelled") {
		t.Fatalf("cancel exit=%d: %s", code, out)
	}
	select {
	case code = <-done:
		if code != 11 || f.launches(t) != 1 {
			t.Fatalf("pending owner exit=%d launches=%d", code, f.launches(t))
		}
	case <-ctx.Done():
		t.Fatal("owner stayed in cancelled backoff")
	}
}

func TestOpenCodeRecoveryDeadlineDoesNotSendFollowup(t *testing.T) {
	f := automaticOpenCodeFixture(t, "rs")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, out := invokeAutomaticOpenCode(ctx, f, func(int, int64) time.Duration {
		return time.Minute
	}, "--wait", "--timeout", "5s")
	if code != 11 || len(openCodeChain(t, out)) != 0 || f.launches(t) != 1 {
		t.Fatalf("deadline exit=%d launches=%d: %s", code, f.launches(t), out)
	}
}

func TestOpenCodeRecoveryClaimExpiryMakesFailureCollectible(t *testing.T) {
	f := automaticOpenCodeFixture(t, "es")
	disableOpenCodeRecovery(t, f, false, true)
	code, out := f.invoke("private-original-task", "--wait")
	if code != 11 {
		t.Fatalf("seed exit=%d: %s", code, out)
	}
	id, err := ids.ParseExecutionID(failureExecutionIDFromOutput(t, out))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	journal, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	row, err := journal.GetExecution(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	row.OpenCodeRecovery = &model.OpenCodeRecovery{Kind: model.OpenCodeFinalAnswer, Attempt: 1, NotBefore: start, ExpiresAt: start.Add(time.Minute), Claimed: true}
	if row, err = journal.UpdateExecution(ctx, row, row.Revision); err != nil {
		t.Fatal(err)
	}
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	for _, tt := range []struct {
		now   time.Time
		state model.State
	}{{start, model.StateWaiting}, {start.Add(time.Minute), model.StateFailed}} {
		a.now = func() time.Time { return tt.now }
		projected, _, err := a.followOpenCodeRecovery(ctx, journal, row)
		if err != nil || projected.State != tt.state {
			t.Fatalf("state=%s want=%s err=%v", projected.State, tt.state, err)
		}
	}
}
