package capacity

import (
	"math"
	"slices"
	"sort"
	"time"
)

// Policy is the configured selection rule. An empty policy uses the default.
type Policy string

const (
	ModeSoonestReset     = "soonest_reset"
	ModeDrainBeforeReset = "drain_before_reset"
)

// HeadroomFloor is the least room, as a share of the window, that a candidate
// must keep below its threshold to stay eligible. It stops an account that is
// nearly full from winning on a short reset.
const HeadroomFloor = 0.05

// Reason says why a candidate was chosen. Values are stable identifiers.
const (
	ReasonSoonestReset     = "soonest_reset"
	ReasonDrainBeforeReset = "drain_before_reset"
	ReasonConfiguredOrder  = "configured_order"
	ReasonAllOverThreshold = "all_over_threshold"
)

// Candidate is one pool entry, reduced to what selection needs.
type Candidate struct {
	Account   string
	Windows   []string
	Threshold float64
	Exempt    bool
}

// Launch is one local pool execution. A nil TerminalAt means work is still in
// flight. Completed work needs a debit only until a usage observation includes it.
type Launch struct {
	Account    string
	TerminalAt *time.Time
}

// Input is everything one choice depends on, so a choice is reproducible from
// it. Facts is nil when the file is unusable; then no account has known usage.
type Input struct {
	Candidates   []Candidate
	Facts        *Facts
	Launches     []Launch
	LaunchDebit  float64
	Policy       Policy
	Epsilon      float64
	ExpectedCost float64
	MaxAge       time.Duration
	Now          time.Time
}

// Evaluation explains how one candidate looked at selection time. Pressure is
// the used ratio plus the debit for recent launches, the figure eligibility uses.
type Evaluation struct {
	Account         string
	UsageKnown      bool
	UsedRatio       float64
	Window          string
	ResetsAt        *time.Time
	Threshold       float64
	Exempt          bool
	Eligible        bool
	RecentLaunches  int
	pressure        float64
	rankingReset    *time.Time
	rankingPressure float64
	unusableWindow  bool
}

// Choice is the selected candidate index, why, and every evaluation in pool order.
type Choice struct {
	Index       int
	Mode        string
	Reason      string
	Evaluations []Evaluation
}

// Select chooses exactly one candidate from a nonempty list. It never refuses: a
// pool blocks no work on a number that may be stale, so when every candidate is
// over its limit the least used one runs and the reason says so.
//
// Drain ranks accounts by their latest usable reset. The legacy policy ranks
// by the limiting window's reset. Candidates with known usage but no known
// reset come next, then candidates with unknown usage.
func Select(input Input) Choice {
	mode := ModeDrainBeforeReset
	if input.Policy == ModeSoonestReset || input.Facts == nil {
		mode = ModeSoonestReset
	}
	evaluations := evaluateCandidates(input, mode)
	if mode == ModeDrainBeforeReset && slices.ContainsFunc(evaluations, func(e Evaluation) bool { return e.unusableWindow }) {
		// A fresh snapshot can carry a stale observation for one window.
		// Use the legacy rule if we cannot check every reported window.
		mode = ModeSoonestReset
		evaluations = evaluateCandidates(input, mode)
	}
	choice := Choice{Mode: mode, Evaluations: evaluations}
	eligible := make([]int, 0, len(evaluations))
	for i, evaluation := range evaluations {
		if evaluation.Eligible {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		choice.Index, choice.Reason = leastUsed(evaluations), ReasonAllOverThreshold
		return choice
	}
	choice.Index = soonestReset(evaluations, eligible)
	switch {
	case evaluations[choice.Index].rankingReset == nil:
		choice.Reason = ReasonConfiguredOrder
	case mode == ModeDrainBeforeReset:
		choice.Reason = ReasonDrainBeforeReset
	default:
		choice.Reason = ReasonSoonestReset
	}
	return choice
}

func evaluateCandidates(input Input, mode string) []Evaluation {
	evaluations := make([]Evaluation, len(input.Candidates))
	for i, candidate := range input.Candidates {
		evaluations[i] = evaluate(candidate, input, mode == ModeDrainBeforeReset)
	}
	return evaluations
}

// evaluate takes the tightest limiting window. A window past its reset counts as
// empty with no known next reset, and one observed longer ago than MaxAge says
// nothing about now.
func evaluate(candidate Candidate, input Input, drain bool) Evaluation {
	evaluation := Evaluation{Account: candidate.Account, Threshold: candidate.Threshold, Exempt: candidate.Exempt}
	if drain {
		evaluation.Threshold = 1 - input.Epsilon
		evaluation.Exempt = false
	}
	// Without usable observations, retain only the bounded fallback history.
	evaluation.RecentLaunches = pendingLaunches(input, candidate.Account, input.Now.Add(-input.MaxAge))
	evaluation.pressure = input.LaunchDebit * float64(evaluation.RecentLaunches)
	if input.Facts != nil {
		for _, account := range input.Facts.Accounts {
			if account.ID != candidate.Account {
				continue
			}
			for _, window := range account.Windows {
				if !drain && !limits(candidate.Windows, window.Name) {
					continue
				}
				observed := input.Facts.GeneratedAt
				if window.ObservedAt != nil {
					observed = *window.ObservedAt
				}
				if input.Now.Sub(observed) > input.MaxAge || observed.After(input.Now.Add(maxClockSkew)) {
					evaluation.unusableWindow = true
					continue
				}
				ratio, resets := window.UsedRatio, window.ResetsAt
				if resets != nil && !resets.After(input.Now) {
					ratio = 0
					if resets.After(observed) {
						observed = *resets
					}
					resets = nil
				}
				// A small tolerated clock skew must not erase reservations for
				// launches that have not happened at selection time.
				if observed.After(input.Now) {
					observed = input.Now
				}
				recent := pendingLaunches(input, candidate.Account, observed)
				pressure := ratio + input.LaunchDebit*float64(recent)
				if drain && resets != nil && (evaluation.rankingReset == nil || resets.After(*evaluation.rankingReset) ||
					(resets.Equal(*evaluation.rankingReset) && pressure > evaluation.rankingPressure)) {
					evaluation.rankingReset, evaluation.rankingPressure = resets, pressure
				}
				if !evaluation.UsageKnown || pressure > evaluation.pressure ||
					(pressure == evaluation.pressure && earlierReset(resets, evaluation.ResetsAt)) {
					evaluation.UsageKnown, evaluation.UsedRatio, evaluation.Window, evaluation.ResetsAt = true, ratio, window.Name, resets
					evaluation.RecentLaunches, evaluation.pressure = recent, pressure
				}
			}
			break
		}
	}
	if !drain {
		evaluation.rankingReset = evaluation.ResetsAt
	}
	if !drain || evaluation.rankingReset == nil {
		evaluation.rankingPressure = evaluation.pressure
	}
	switch {
	case evaluation.Exempt || !evaluation.UsageKnown:
		evaluation.Eligible = true
	case drain:
		// The highest pressure tests the ceiling for all windows. One ULP
		// covers floating-point rounding at the inclusive boundary.
		limit := math.Nextafter(evaluation.Threshold, math.Inf(1))
		evaluation.Eligible = evaluation.pressure+input.ExpectedCost <= limit
	default:
		// Add the headroom before comparing, avoiding cancellation at small
		// thresholds. One ULP covers rounding at the inclusive boundary.
		limit := math.Nextafter(candidate.Threshold, math.Inf(1))
		evaluation.Eligible = evaluation.pressure < candidate.Threshold && evaluation.pressure+HeadroomFloor <= limit
	}
	return evaluation
}

func pendingLaunches(input Input, account string, observed time.Time) int {
	count := 0
	for _, launch := range input.Launches {
		if launch.Account == account && (launch.TerminalAt == nil || launch.TerminalAt.After(observed)) {
			count++
		}
	}
	return count
}

func earlierReset(a, b *time.Time) bool {
	return a != nil && (b == nil || a.Before(*b))
}

func limits(names []string, window string) bool {
	if len(names) == 0 {
		return true
	}
	for _, name := range names {
		if name == window {
			return true
		}
	}
	return false
}

// soonestReset ranks eligible candidates: known usage with a ranking reset first,
// soonest reset first and lower pressure on a tie; then known usage without a
// reset; then unknown usage. Remaining ties keep configured order.
func soonestReset(evaluations []Evaluation, indexes []int) int {
	ranked := append([]int(nil), indexes...)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := evaluations[ranked[i]], evaluations[ranked[j]]
		if ta, tb := tier(a), tier(b); ta != tb {
			return ta < tb
		}
		if a.rankingReset != nil && b.rankingReset != nil && !a.rankingReset.Equal(*b.rankingReset) {
			return a.rankingReset.Before(*b.rankingReset)
		}
		return a.rankingPressure < b.rankingPressure
	})
	return ranked[0]
}

// leastUsed picks the lowest pressure, with unknown usage last. It runs only
// when every candidate is over its limit.
func leastUsed(evaluations []Evaluation) int {
	ranked := make([]int, len(evaluations))
	for i := range ranked {
		ranked[i] = i
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := evaluations[ranked[i]], evaluations[ranked[j]]
		if a.UsageKnown != b.UsageKnown {
			return a.UsageKnown
		}
		return a.pressure < b.pressure
	})
	return ranked[0]
}

// tier orders candidates for soonestReset: 0 known usage with a reset, 1 known
// usage without one, 2 unknown usage.
func tier(e Evaluation) int {
	switch {
	case e.UsageKnown && e.rankingReset != nil:
		return 0
	case e.UsageKnown:
		return 1
	default:
		return 2
	}
}
