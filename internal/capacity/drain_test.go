package capacity

import (
	"testing"
	"time"
)

func drainInput(facts *Facts, candidates ...Candidate) Input {
	in := input(facts, candidates...)
	in.Policy, in.Epsilon, in.ExpectedCost = "", .03, .01
	return in
}

func TestDrainBeforeResetNovemberFacts(t *testing.T) {
	facts, err := DecodeFacts([]byte(`{"schema_version":1,"generated_at":"2026-11-24T05:30:00Z","accounts":[
		{"id":"claude-1","windows":[{"name":"weekly","used_ratio":0.90,"resets_at":"2026-11-25T00:00:00Z"}]},
		{"id":"claude-2","windows":[{"name":"5h","used_ratio":0.38,"resets_at":"2026-11-24T07:00:00Z"},{"name":"weekly","used_ratio":0.21,"resets_at":"2026-11-28T00:00:00Z"}]},
		{"id":"codex","windows":[{"name":"weekly","used_ratio":0.90,"resets_at":"2026-11-26T00:00:00Z"}]},
		{"id":"codex-2","windows":[{"name":"weekly","used_ratio":0.16,"resets_at":"2026-11-29T00:00:00Z"}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	claude := drainInput(&facts, Candidate{Account: "claude-2", Threshold: .95}, Candidate{Account: "claude-1", Threshold: .95})
	claude.Now = facts.GeneratedAt
	if got := Select(claude); got.Index != 1 || got.Mode != ModeDrainBeforeReset || got.Reason != ReasonDrainBeforeReset {
		t.Fatalf("Claude drain choice: %+v", got)
	}
	claude.Policy = ModeSoonestReset
	if got := Select(claude); got.Index != 0 || got.Mode != ModeSoonestReset || got.Reason != ReasonSoonestReset {
		t.Fatalf("Claude legacy choice: %+v", got)
	}
	codex := drainInput(&facts, Candidate{Account: "codex-2"}, Candidate{Account: "codex"})
	codex.Now = facts.GeneratedAt
	if got := Select(codex); got.Index != 1 {
		t.Fatalf("Codex drain choice: %+v", got)
	}
	facts.Accounts[2].Windows[0].UsedRatio = .975
	if got := Select(codex); got.Index != 0 || got.Evaluations[1].Eligible {
		t.Fatalf("Codex near limit: %+v", got)
	}
}

func TestDrainChecksEveryWindowAtProjectedBoundary(t *testing.T) {
	soon := testNow.Add(time.Hour)
	a := weekly("a", .2)
	a.Windows = append(a.Windows, Window{Name: "5h", UsedRatio: .94, ResetsAt: &soon})
	in := drainInput(factsWith(a, weekly("b", .99)), Candidate{Account: "a", Windows: []string{"weekly"}, Exempt: true}, Candidate{Account: "b"})
	in.Launches = []Launch{{Account: "a"}}
	if got := Select(in); got.Index != 0 || !got.Evaluations[0].Eligible || got.Evaluations[0].Window != "5h" {
		t.Fatalf("inclusive boundary (0.94 + 0.02 + 0.01): %+v", got)
	}
	a.Windows[1].UsedRatio = .940001
	if got := Select(in); got.Index != 0 || got.Evaluations[0].Eligible || got.Reason != ReasonAllOverThreshold {
		t.Fatalf("short window above the ceiling and all-over fallback: %+v", got)
	}
	in.Epsilon, in.ExpectedCost = .02, .02
	a.Windows[1].UsedRatio = .94
	if got := Select(in); !got.Evaluations[0].Eligible {
		t.Fatalf("configured boundary (0.94 + 0.02 + 0.02): %+v", got)
	}
}

func TestDrainBreaksLatestResetTieByPressureThenConfiguredOrder(t *testing.T) {
	soon := testNow.Add(time.Hour)
	a, b := weekly("a", .2), weekly("b", .3)
	a.Windows = append(a.Windows, Window{Name: "5h", UsedRatio: .5, ResetsAt: &soon})
	in := drainInput(factsWith(a, b), Candidate{Account: "b"}, Candidate{Account: "a"})
	in.Launches = []Launch{{Account: "a"}}
	if got := Select(in); got.Index != 1 {
		t.Fatalf("latest window pressure should beat pool order and short window pressure: %+v", got)
	}
	a.Windows[0].UsedRatio, b.Windows[0].UsedRatio = .10, .12
	in.Candidates[0], in.Candidates[1] = in.Candidates[1], in.Candidates[0]
	if got := Select(in); got.Index != 0 {
		t.Fatalf("0.10 + 0.02 and 0.12 tie, so the first account should win: %+v", got)
	}
	a.Windows[1].ResetsAt = a.Windows[0].ResetsAt
	if got := Select(in); got.Index != 1 {
		t.Fatalf("highest pressure among windows at the same latest reset should break the tie: %+v", got)
	}
}

func TestDrainKeepsDecimalCeilingInclusiveWithMultipleDebits(t *testing.T) {
	a := weekly("a", .81)
	later := testNow.Add(96 * time.Hour)
	b := Account{ID: "b", Windows: []Window{{Name: "weekly", UsedRatio: .50, ResetsAt: &later}}}
	in := drainInput(factsWith(a, b), Candidate{Account: "a"}, Candidate{Account: "b"})
	in.Epsilon, in.ExpectedCost = .07, .06
	in.Launches = []Launch{{Account: "a"}, {Account: "a"}, {Account: "a"}}
	if got := Select(in); got.Index != 0 || !got.Evaluations[0].Eligible || got.Evaluations[0].Threshold != .93 {
		t.Fatalf("0.81 + 3*0.02 + 0.06 is exactly 0.93: %+v", got)
	}
	a.Windows[0].UsedRatio = .8100000000000002
	if got := Select(in); got.Index != 1 || got.Evaluations[0].Eligible {
		t.Fatalf("a usage value just above the ceiling should select the other account: %+v", got)
	}
}

func TestDrainUsesLegacyPolicyWhenAWindowIsStale(t *testing.T) {
	stale := testNow.Add(-time.Hour - time.Second)
	a := weekly("a", .91)
	a.Windows = append(a.Windows, Window{Name: "5h", UsedRatio: .1, ObservedAt: &stale})
	in := drainInput(factsWith(a, weekly("b", .5)), Candidate{Account: "a", Threshold: .95}, Candidate{Account: "b", Threshold: .95})
	if got := Select(in); got.Mode != ModeSoonestReset || got.Evaluations[0].Eligible || got.Index != 1 {
		t.Fatalf("stale window should select with the legacy policy: %+v", got)
	}
}
