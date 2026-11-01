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

func input(mode string, facts *Facts, candidates ...Candidate) Input {
	return Input{Mode: mode, Candidates: candidates, Facts: facts, LaunchDebit: 0.02, MaxAge: time.Hour, Now: testNow}
}

func TestPriorityDrainsFirstAccountWhileExempt(t *testing.T) {
	choice := Select(input(ModePriority, factsWith(weekly("a", 1.4), weekly("b", 0)),
		Candidate{Account: "a", Exempt: true, Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}))
	if choice.Index != 0 || choice.Reason != ReasonFirstEligible {
		t.Fatalf("choice=%+v", choice)
	}
}

func TestPriorityMovesOnAtThreshold(t *testing.T) {
	choice := Select(input(ModePriority, factsWith(weekly("a", 0.95), weekly("b", 0.99)),
		Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Exempt: true}))
	if choice.Index != 1 {
		t.Fatalf("choice=%+v", choice)
	}
	if !Select(input(ModePriority, factsWith(weekly("a", 0.94)), Candidate{Account: "a", Threshold: 0.95})).Evaluations[0].Eligible {
		t.Fatal("below the threshold must stay eligible")
	}
}

func TestBalancePicksMostHeadroomAndSkipsOverThreshold(t *testing.T) {
	choice := Select(input(ModeBalance, factsWith(weekly("a", 0.96), weekly("b", 0.5), weekly("c", 0.2)),
		Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}, Candidate{Account: "c", Threshold: 0.95}))
	if choice.Index != 2 || choice.Reason != ReasonLowestPressure {
		t.Fatalf("choice=%+v", choice)
	}
}

func TestBalanceSpreadsFanOutWhenFactsLag(t *testing.T) {
	in := input(ModeBalance, factsWith(weekly("a", 0.1), weekly("b", 0.1)), Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95})
	in.RecentLaunches = map[string]int{"a": 1}
	if got := Select(in).Index; got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestBalanceTieGoesToSoonestReset(t *testing.T) {
	soon, late := testNow.Add(time.Hour), testNow.Add(100*time.Hour)
	facts := factsWith(Account{ID: "a", Windows: []Window{{Name: "w", UsedRatio: 0.3, ResetsAt: &late}}}, Account{ID: "b", Windows: []Window{{Name: "w", UsedRatio: 0.3, ResetsAt: &soon}}})
	if got := Select(input(ModeBalance, facts, Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95})).Index; got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestBalanceRanksUnknownUsageLast(t *testing.T) {
	if got := Select(input(ModeBalance, factsWith(weekly("b", 0.8)), Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95})).Index; got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestAllOverThresholdRunsLeastUsed(t *testing.T) {
	choice := Select(input(ModePriority, factsWith(weekly("a", 0.99), weekly("b", 0.96)), Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}))
	if choice.Index != 1 || choice.Reason != ReasonAllOverThreshold {
		t.Fatalf("choice=%+v", choice)
	}
}

func TestNoFactsKeepsOrderAndBalanceStillSpreads(t *testing.T) {
	a, b := Candidate{Account: "a", Threshold: 0.95}, Candidate{Account: "b", Threshold: 0.95}
	if got := Select(input(ModePriority, nil, a, b)); got.Index != 0 || got.Evaluations[0].UsageKnown {
		t.Fatalf("choice=%+v", got)
	}
	in := input(ModeBalance, nil, a, b)
	in.RecentLaunches = map[string]int{"a": 2, "b": 1}
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
	got := Select(input(ModePriority, facts, Candidate{Account: "a", Threshold: 0.95, Windows: []string{"reset", "aged", "live"}})).Evaluations[0]
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
