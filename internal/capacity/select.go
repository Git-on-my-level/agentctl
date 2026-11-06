package capacity

import (
	"sort"
	"time"
)

// ModeSoonestReset names the one selection rule. It is recorded with every
// choice so a reader can tell which rule chose the account.
const ModeSoonestReset = "soonest_reset"

// HeadroomFloor is the least room, as a share of the window, that a candidate
// must keep below its threshold to stay eligible. It stops an account that is
// nearly full from winning on a short reset.
const HeadroomFloor = 0.05

// Reason says why a candidate was chosen. Values are stable identifiers.
const (
	ReasonSoonestReset     = "soonest_reset"
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

// Input is everything one choice depends on, so a choice is reproducible from
// it. Facts is nil when the file is unusable; then no account has known usage.
type Input struct {
	Candidates     []Candidate
	Facts          *Facts
	RecentLaunches map[string]int
	LaunchDebit    float64
	MaxAge         time.Duration
	Now            time.Time
}

// Evaluation explains how one candidate looked at selection time. Pressure is
// the used ratio plus the debit for recent launches, the figure eligibility uses.
type Evaluation struct {
	Account        string
	UsageKnown     bool
	UsedRatio      float64
	Window         string
	ResetsAt       *time.Time
	Threshold      float64
	Exempt         bool
	Eligible       bool
	RecentLaunches int
	pressure       float64
}

// Choice is the selected candidate index, why, and every evaluation in pool order.
type Choice struct {
	Index       int
	Reason      string
	Evaluations []Evaluation
}

// Select chooses exactly one candidate from a nonempty list. It never refuses: a
// pool blocks no work on a number that may be stale, so when every candidate is
// over its limit the least used one runs and the reason says so.
//
// Among eligible candidates, the one whose window resets soonest runs first,
// because unused quota in that window is lost at reset. Candidates with known
// usage but no known reset come next, then candidates with unknown usage. Ties
// keep configured order.
func Select(input Input) Choice {
	evaluations := make([]Evaluation, len(input.Candidates))
	for i, candidate := range input.Candidates {
		evaluations[i] = evaluate(candidate, input)
	}
	choice := Choice{Evaluations: evaluations}
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
	if evaluations[choice.Index].ResetsAt != nil {
		choice.Reason = ReasonSoonestReset
	} else {
		choice.Reason = ReasonConfiguredOrder
	}
	return choice
}

// evaluate takes the tightest limiting window. A window past its reset counts as
// empty with no known next reset, and one observed longer ago than MaxAge says
// nothing about now.
func evaluate(candidate Candidate, input Input) Evaluation {
	evaluation := Evaluation{Account: candidate.Account, Threshold: candidate.Threshold, Exempt: candidate.Exempt, RecentLaunches: input.RecentLaunches[candidate.Account]}
	if input.Facts != nil {
		for _, account := range input.Facts.Accounts {
			if account.ID != candidate.Account {
				continue
			}
			for i := range account.Windows {
				window := account.Windows[i]
				if !limits(candidate.Windows, window.Name) {
					continue
				}
				if window.ObservedAt != nil && input.Now.Sub(*window.ObservedAt) > input.MaxAge {
					continue
				}
				ratio, resets := window.UsedRatio, window.ResetsAt
				if resets != nil && !resets.After(input.Now) {
					ratio, resets = 0, nil
				}
				if !evaluation.UsageKnown || ratio > evaluation.UsedRatio {
					evaluation.UsageKnown, evaluation.UsedRatio, evaluation.Window, evaluation.ResetsAt = true, ratio, window.Name, resets
				}
			}
			break
		}
	}
	evaluation.pressure = evaluation.UsedRatio + input.LaunchDebit*float64(evaluation.RecentLaunches)
	switch {
	case candidate.Exempt || !evaluation.UsageKnown:
		evaluation.Eligible = true
	default:
		evaluation.Eligible = evaluation.pressure < candidate.Threshold && candidate.Threshold-evaluation.pressure >= HeadroomFloor
	}
	return evaluation
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

// soonestReset ranks eligible candidates: known usage with a known reset first,
// soonest reset first and lower pressure on a tie; then known usage without a
// reset; then unknown usage. Remaining ties keep configured order.
func soonestReset(evaluations []Evaluation, indexes []int) int {
	ranked := append([]int(nil), indexes...)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := evaluations[ranked[i]], evaluations[ranked[j]]
		if ta, tb := tier(a), tier(b); ta != tb {
			return ta < tb
		}
		if a.ResetsAt != nil && b.ResetsAt != nil && !a.ResetsAt.Equal(*b.ResetsAt) {
			return a.ResetsAt.Before(*b.ResetsAt)
		}
		return a.pressure < b.pressure
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
	case e.UsageKnown && e.ResetsAt != nil:
		return 0
	case e.UsageKnown:
		return 1
	default:
		return 2
	}
}
