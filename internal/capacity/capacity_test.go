package capacity

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func factsWith(accounts ...Account) *Facts {
	return &Facts{SchemaVersion: 1, GeneratedAt: testNow, Accounts: accounts}
}

func weekly(id string, ratio float64) Account {
	reset := testNow.Add(72 * time.Hour)
	return Account{ID: id, Windows: []Window{{Name: "weekly", UsedRatio: ratio, ResetsAt: &reset}}}
}

func input(facts *Facts, candidates ...Candidate) Input {
	return Input{Candidates: candidates, Facts: facts, LaunchDebit: 0.02, MaxAge: time.Hour, Now: testNow}
}

func TestSoonestResetWinsOverFresherAccount(t *testing.T) {
	// 86% used and 17h to reset, against a fresh account with a full week left.
	soon := testNow.Add(17 * time.Hour)
	used := Account{ID: "a", Windows: []Window{{Name: "weekly", UsedRatio: 0.86, ResetsAt: &soon}}}
	choice := Select(input(factsWith(used, weekly("b", 0)), Candidate{Account: "a", Threshold: 0.96}, Candidate{Account: "b", Threshold: 0.96}))
	if choice.Index != 0 || choice.Reason != ReasonSoonestReset {
		t.Fatalf("choice=%+v", choice)
	}
}

func TestHeadroomFloorExcludesNearlyFullAccount(t *testing.T) {
	soon := testNow.Add(time.Hour)
	nearly := Account{ID: "a", Windows: []Window{{Name: "weekly", UsedRatio: 0.91, ResetsAt: &soon}}}
	choice := Select(input(factsWith(nearly, weekly("b", 0.5)), Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}))
	if choice.Index != 1 || choice.Evaluations[0].Eligible {
		t.Fatalf("choice=%+v", choice)
	}
	if !Select(input(factsWith(weekly("a", 0.85)), Candidate{Account: "a", Threshold: 0.95})).Evaluations[0].Eligible {
		t.Fatal("headroom of 5% or more must stay eligible")
	}
}

func TestExemptRunsPastLimit(t *testing.T) {
	choice := Select(input(factsWith(weekly("a", 1.4), weekly("b", 0)),
		Candidate{Account: "a", Exempt: true, Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}))
	if !choice.Evaluations[0].Eligible {
		t.Fatalf("choice=%+v", choice)
	}
}

func TestResetOrderBreaksAcrossKnownUsageThenUnknown(t *testing.T) {
	soon, late := testNow.Add(time.Hour), testNow.Add(100*time.Hour)
	facts := factsWith(
		Account{ID: "a", Windows: []Window{{Name: "w", UsedRatio: 0.2, ResetsAt: &late}}},
		Account{ID: "b", Windows: []Window{{Name: "w", UsedRatio: 0.2, ResetsAt: &soon}}},
		Account{ID: "c", Windows: []Window{{Name: "w", UsedRatio: 0.1}}},
	)
	choice := Select(input(facts, Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "c", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}))
	if choice.Index != 2 {
		t.Fatalf("choice=%+v", choice)
	}
	choice = Select(input(facts, Candidate{Account: "c", Threshold: 0.95}, Candidate{Account: "a", Threshold: 0.95}))
	if choice.Index != 1 || choice.Reason != ReasonSoonestReset {
		t.Fatalf("choice=%+v", choice)
	}
}

func TestExpiredWindowCountsEmptyWithoutNextReset(t *testing.T) {
	past := testNow.Add(-time.Minute)
	expired := Account{ID: "a", Windows: []Window{{Name: "w", UsedRatio: 0.9, ResetsAt: &past}}}
	later := testNow.Add(72 * time.Hour)
	fresh := Account{ID: "b", Windows: []Window{{Name: "w", UsedRatio: 0.5, ResetsAt: &later}}}
	choice := Select(input(factsWith(expired, fresh), Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}))
	got := choice.Evaluations[0]
	if got.UsedRatio != 0 || got.ResetsAt != nil || !got.Eligible {
		t.Fatalf("evaluation=%+v", got)
	}
	if choice.Index != 1 || choice.Reason != ReasonSoonestReset {
		t.Fatalf("choice=%+v", choice)
	}
}

func TestAllOverThresholdRunsLeastUsed(t *testing.T) {
	choice := Select(input(factsWith(weekly("a", 0.99), weekly("b", 0.96)), Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}))
	if choice.Index != 1 || choice.Reason != ReasonAllOverThreshold {
		t.Fatalf("choice=%+v", choice)
	}
}

func TestLaunchDebitSpreadsFanOutWhenFactsLag(t *testing.T) {
	in := input(factsWith(weekly("a", 0.1), weekly("b", 0.1)), Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95})
	in.Launches = []Launch{{Account: "a"}}
	if got := Select(in).Index; got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestNoFactsKeepsConfiguredOrder(t *testing.T) {
	a, b := Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}
	if got := Select(input(nil, a, b)); got.Index != 0 || got.Evaluations[0].UsageKnown || got.Reason != ReasonConfiguredOrder {
		t.Fatalf("choice=%+v", got)
	}
	in := input(nil, a, b)
	in.Launches = []Launch{{Account: "a"}, {Account: "a"}, {Account: "b"}}
	if got := Select(in).Index; got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestWindowsAreFilteredResetAndAged(t *testing.T) {
	past, old := testNow.Add(-time.Minute), testNow.Add(-2*time.Hour)
	facts := factsWith(Account{ID: "a", Windows: []Window{
		{Name: "other", UsedRatio: 1},
		{Name: "reset", UsedRatio: 0.99, ResetsAt: &past},
		{Name: "aged", UsedRatio: 0.99, ObservedAt: &old},
		{Name: "live", UsedRatio: 0.4},
	}})
	got := Select(input(facts, Candidate{Account: "a", Threshold: 0.95, Windows: []string{"reset", "aged", "live"}})).Evaluations[0]
	if !got.Eligible || got.UsedRatio != 0.4 || got.Window != "live" {
		t.Fatalf("evaluation=%+v", got)
	}
}

func TestLoadDoesNotHangOnFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Skip(err)
	}
	done := make(chan string, 1)
	go func() {
		_, status := Load(path, time.Hour, testNow)
		done <- status
	}()
	select {
	case status := <-done:
		if status != StatusInvalid {
			t.Fatalf("status=%s", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}

func TestLoadReportsEveryStatus(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := `{"schema_version":1,"generated_at":"2026-11-01T11:30:00Z","extra":true,"accounts":[{"id":"a","windows":[{"name":"w","used_ratio":1.2}]}]}`
	cases := map[string]struct {
		path string
		want string
	}{
		"ok":             {write("ok", good), StatusOK},
		"not configured": {"", StatusNotConfigured},
		"missing":        {filepath.Join(dir, "absent"), StatusMissing},
		"stale":          {write("stale", strings.Replace(good, "11:30", "09:00", 1)), StatusStale},
		"future":         {write("future", strings.Replace(good, "11:30", "13:00", 1)), StatusInvalid},
		"not json":       {write("bad", "{"), StatusInvalid},
		"version":        {write("version", strings.Replace(good, `"schema_version":1`, `"schema_version":2`, 1)), StatusInvalid},
		"negative":       {write("negative", strings.Replace(good, "1.2", "-1", 1)), StatusInvalid},
		"repeat":         {write("repeat", strings.Replace(good, `"accounts":[`, `"accounts":[{"id":"a"},`, 1)), StatusInvalid},
		"too large":      {write("large", strings.Repeat(" ", MaxFactsBytes+1)+good), StatusInvalid},
		"directory":      {dir, StatusInvalid},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, got := Load(tc.path, time.Hour, testNow); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestLaunchDebitsRetireAtObservation(t *testing.T) {
	for _, account := range []string{"codex-2", "claude-2", "cursor-2"} {
		t.Run(account, func(t *testing.T) {
			observed := testNow.Add(-10 * time.Minute)
			soon := testNow.Add(14 * time.Hour)
			facts := factsWith(weekly("primary", .29), Account{ID: account, Windows: []Window{
				{Name: "weekly", UsedRatio: .68, ResetsAt: &soon, ObservedAt: &observed},
			}})
			in := input(facts, Candidate{Account: "primary", Threshold: .95}, Candidate{Account: account, Threshold: .95})
			for i := 0; i < 11; i++ {
				// Includes equality: an observation already includes that completion.
				ended := observed.Add(-time.Duration(i) * time.Minute)
				in.Launches = append(in.Launches, Launch{Account: account, TerminalAt: &ended})
			}
			got := Select(in)
			if got.Index != 1 || got.Evaluations[1].RecentLaunches != 0 || !got.Evaluations[1].Eligible {
				t.Fatalf("measured work debited again: %+v", got)
			}
			// Retain both running work and work finished after the observation.
			ended := observed.Add(time.Minute)
			in.Launches = append(in.Launches, Launch{Account: account}, Launch{Account: account, TerminalAt: &ended})
			if got := Select(in); got.Index != 1 || got.Evaluations[1].RecentLaunches != 2 {
				t.Fatalf("unobserved work lost its debit: %+v", got)
			}
		})
	}
}

func TestEachWindowUsesItsOwnObservationAndPressure(t *testing.T) {
	old, fresh := testNow.Add(-50*time.Minute), testNow.Add(-time.Minute)
	ended := testNow.Add(-10 * time.Minute)
	soon, late := testNow.Add(time.Hour), testNow.Add(6*24*time.Hour)
	facts := factsWith(Account{ID: "a", Windows: []Window{
		{Name: "weekly", UsedRatio: .80, ObservedAt: &fresh, ResetsAt: &late},
		{Name: "5h", UsedRatio: .79, ObservedAt: &old, ResetsAt: &soon},
	}})
	in := input(facts, Candidate{Account: "a", Threshold: .95})
	in.Launches = []Launch{{Account: "a", TerminalAt: &ended}}
	got := Select(in).Evaluations[0]
	if got.Window != "5h" || got.RecentLaunches != 1 || got.UsedRatio != .79 {
		t.Fatalf("limiting pressure ignored window observation: %+v", got)
	}
	in.Candidates[0].Windows = []string{"weekly"}
	got = Select(in).Evaluations[0]
	if got.Window != "weekly" || got.RecentLaunches != 0 {
		t.Fatalf("filtered window changed debit: %+v", got)
	}
}

func TestGeneratedAtIsFallbackObservation(t *testing.T) {
	ended := testNow.Add(-time.Minute)
	in := input(factsWith(weekly("a", .68)), Candidate{Account: "a", Threshold: .95})
	in.Launches = []Launch{{Account: "a", TerminalAt: &ended}}
	if got := Select(in).Evaluations[0]; got.RecentLaunches != 0 {
		t.Fatalf("generated_at did not retire debit: %+v", got)
	}
}

func TestResetRetiresPriorWindowDebits(t *testing.T) {
	observed, reset, ended := testNow.Add(-30*time.Minute), testNow.Add(-10*time.Minute), testNow.Add(-20*time.Minute)
	in := input(factsWith(Account{ID: "a", Windows: []Window{{Name: "w", UsedRatio: .99, ObservedAt: &observed, ResetsAt: &reset}}}), Candidate{Account: "a", Threshold: .95})
	in.Launches = []Launch{{Account: "a", TerminalAt: &ended}, {Account: "a"}}
	if got := Select(in).Evaluations[0]; got.UsedRatio != 0 || got.RecentLaunches != 1 || !got.Eligible {
		t.Fatalf("reset retained old-window debits: %+v", got)
	}
}

func TestUnknownUsageBoundsCompletedHistoryAndKeepsRunningWork(t *testing.T) {
	old, fresh := testNow.Add(-2*time.Hour), testNow.Add(-time.Minute)
	in := input(nil, Candidate{Account: "a", Threshold: .95})
	in.Launches = []Launch{{Account: "a", TerminalAt: &old}, {Account: "a", TerminalAt: &fresh}, {Account: "a"}, {Account: "other"}}
	if got := Select(in).Evaluations[0]; got.RecentLaunches != 2 || got.UsageKnown {
		t.Fatalf("fallback debit: %+v", got)
	}
}

func TestHeadroomBoundaryIsInclusive(t *testing.T) {
	for _, tc := range []struct {
		ratio    float64
		eligible bool
	}{{.899999, true}, {.90, true}, {.900001, false}, {.95, false}} {
		got := Select(input(factsWith(weekly("a", tc.ratio)), Candidate{Account: "a", Threshold: .95})).Evaluations[0]
		if got.Eligible != tc.eligible {
			t.Fatalf("ratio=%v eligible=%v want=%v", tc.ratio, got.Eligible, tc.eligible)
		}
	}
	in := input(factsWith(weekly("a", .68)), Candidate{Account: "a", Threshold: .95})
	for i := 0; i < 11; i++ {
		in.Launches = append(in.Launches, Launch{Account: "a"})
	}
	if got := Select(in).Evaluations[0]; !got.Eligible {
		t.Fatalf("exact .90 launch pressure rejected: %+v", got)
	}
}

func TestEqualPressureWindowsPreferSoonestResetRegardlessOfOrder(t *testing.T) {
	soon, late := testNow.Add(time.Hour), testNow.Add(72*time.Hour)
	windows := []Window{{Name: "weekly", UsedRatio: .3, ResetsAt: &late}, {Name: "5h", UsedRatio: .3, ResetsAt: &soon}}
	for i := 0; i < 2; i++ {
		got := Select(input(factsWith(Account{ID: "a", Windows: windows}), Candidate{Account: "a", Threshold: .95})).Evaluations[0]
		if got.Window != "5h" {
			t.Fatalf("window order changed limiting reset: %+v", got)
		}
		windows[0], windows[1] = windows[1], windows[0]
	}
}

func TestFutureObservationCannotHidePressure(t *testing.T) {
	future := testNow.Add(time.Hour)
	in := input(factsWith(Account{ID: "a", Windows: []Window{{Name: "w", UsedRatio: .1, ObservedAt: &future}}}), Candidate{Account: "a", Threshold: .95})
	if got := Select(in).Evaluations[0]; got.UsageKnown {
		t.Fatalf("future observation trusted: %+v", got)
	}
}

func TestConfiguredHeadroomBoundary(t *testing.T) {
	for _, tc := range []struct{ threshold, ratio float64 }{{.06, .01}, {.07, .02}, {.10, .05}, {.15, .10}, {.95, .90}} {
		in := input(factsWith(weekly("a", tc.ratio)), Candidate{Account: "a", Threshold: tc.threshold})
		if got := Select(in).Evaluations[0]; !got.Eligible {
			t.Fatalf("exact headroom rejected at threshold=%v pressure=%v", tc.threshold, tc.ratio)
		}
		in.Facts.Accounts[0].Windows[0].UsedRatio += .000001
		if got := Select(in).Evaluations[0]; got.Eligible {
			t.Fatalf("insufficient headroom accepted at threshold=%v", tc.threshold)
		}
	}
}
