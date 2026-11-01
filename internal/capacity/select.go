package capacity

import (
	"sort"
	"time"
)

const (
	ModePriority = "priority"
	ModeBalance  = "balance"
)

// Reason says why a candidate was chosen. Values are stable identifiers.
const (
	ReasonFirstEligible    = "first_eligible"
	ReasonLowestPressure   = "lowest_pressure"
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
	Mode           string
	Candidates     []Candidate
	Facts          *Facts
	RecentLaunches map[string]int
	LaunchDebit    float64
	MaxAge         time.Duration
	Now            time.Time
}

// Evaluation explains how one candidate looked at selection time.
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
// over its threshold the least used one runs and the reason says so.
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
		choice.Index, choice.Reason = lowestPressure(evaluations, allIndexes(len(evaluations))), ReasonAllOverThreshold
		return choice
	}
	if input.Mode == ModeBalance {
		choice.Index, choice.Reason = lowestPressure(evaluations, eligible), ReasonLowestPressure
		return choice
	}
	choice.Index, choice.Reason = eligible[0], ReasonFirstEligible
	return choice
}

// evaluate takes the tightest limiting window. A window past its reset counts as
// empty, and one observed longer ago than MaxAge says nothing about now.
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
				ratio := window.UsedRatio
				if window.ResetsAt != nil && !window.ResetsAt.After(input.Now) {
					ratio = 0
				}
				if !evaluation.UsageKnown || ratio > evaluation.UsedRatio {
					evaluation.UsageKnown, evaluation.UsedRatio, evaluation.Window, evaluation.ResetsAt = true, ratio, window.Name, window.ResetsAt
				}
			}
			break
		}
	}
	evaluation.Eligible = candidate.Exempt || !evaluation.UsageKnown || evaluation.UsedRatio < candidate.Threshold
	evaluation.pressure = evaluation.UsedRatio + input.LaunchDebit*float64(evaluation.RecentLaunches)
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

// lowestPressure prefers known usage, then the least pressure, then the soonest
// reset because that quota is lost otherwise, then configured order.
func lowestPressure(evaluations []Evaluation, indexes []int) int {
	ranked := append([]int(nil), indexes...)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := evaluations[ranked[i]], evaluations[ranked[j]]
		if a.UsageKnown != b.UsageKnown {
			return a.UsageKnown
		}
		if a.pressure != b.pressure {
			return a.pressure < b.pressure
		}
		if a.ResetsAt != nil && b.ResetsAt != nil {
			return a.ResetsAt.Before(*b.ResetsAt)
		}
		return a.ResetsAt != nil && b.ResetsAt == nil
	})
	return ranked[0]
}

func allIndexes(n int) []int {
	indexes := make([]int, n)
	for i := range indexes {
		indexes[i] = i
	}
	return indexes
}
