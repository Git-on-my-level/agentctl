package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/capacity"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/delegation"
	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// recentLaunchScanLimit bounds the journal read a pool does before launching. A
// slow read only costs the fan-out spread, which the warning reports.
const recentLaunchScanLimit = 3 * time.Second

// poolSelection is one pool's outcome. The zero value means no pool was named,
// so a request naming a family or model takes today's path unchanged.
type poolSelection struct {
	resolution delegation.Resolution
	executable string
	capacity   *model.DelegationCapacity
	warnings   []output.Warning
}

type viablePoolCandidate struct {
	candidate  config.CapacityCandidate
	resolution delegation.Resolution
}

// selectDelegatePool picks one account from published facts. Only a name that
// can't be served fails: unusable facts degrade to configured order with a warning.
func (a *app) selectDelegatePool(ctx context.Context, c common, profile config.Profile, selector delegation.Selector) (poolSelection, *output.Error) {
	var pools map[string]config.CapacityPool
	if profile.Capacity != nil {
		pools = profile.Capacity.Pools
	}
	pool, ok := pools[selector.Pool]
	if !ok {
		names := make([]string, 0, len(pools))
		for name := range pools {
			names = append(names, name)
		}
		sort.Strings(names)
		return poolSelection{}, delegateError(output.CodeUsage, "delegate_unknown_pool", "selector.pool names no configured pool").WithDetail("configured_pools", names)
	}
	selection := poolSelection{}
	viable := make([]viablePoolCandidate, 0, len(pool.Candidates))
	entries := delegateEntries(profile)
	for _, candidate := range pool.Candidates {
		resolution, err := resolvePoolCandidate(candidate, entries)
		if err != nil {
			selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_candidate_unresolved",
				Message: "a pool candidate has no matching agent_preferences entry and was skipped", Details: map[string]any{"pool": selector.Pool, "account": candidate.Account, "model": candidate.Model}})
			continue
		}
		viable = append(viable, viablePoolCandidate{candidate: candidate, resolution: resolution})
	}
	if len(viable) == 0 {
		return poolSelection{}, delegateError(output.CodeCapabilityUnavailable, "delegate_pool_no_candidate", "no pool candidate matches this profile's agent_preferences").WithDetail("pool", selector.Pool)
	}

	settings := *profile.Capacity
	now := a.now()
	maxAge := time.Duration(settings.MaxAge()) * time.Second
	facts, status := capacity.Load(settings.File, maxAge, now)
	var age *int64
	if facts != nil {
		seconds := int64(now.Sub(facts.GeneratedAt) / time.Second)
		age = &seconds
	}
	if status != capacity.StatusOK {
		facts = nil
	}
	if status == capacity.StatusMissing || status == capacity.StatusStale || status == capacity.StatusInvalid {
		selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_facts_unusable",
			Message: "capacity facts are unusable, so the pool used its configured order", Details: map[string]any{"pool": selector.Pool, "facts_status": status}})
	}
	recent, recentProblem := a.recentPoolLaunches(ctx, c, now, maxAge)
	if recentProblem != nil {
		selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_recent_launches_unread",
			Message: "recent launches could not be read, so the choice ignored them", Details: map[string]any{"pool": selector.Pool}})
	}

	mode := pool.Mode
	if mode == "" {
		mode = capacity.ModePriority
	}
	input := capacity.Input{Mode: mode, Facts: facts, RecentLaunches: recent, LaunchDebit: settings.Debit(), MaxAge: maxAge, Now: now}
	for _, item := range viable {
		input.Candidates = append(input.Candidates, capacity.Candidate{Account: item.candidate.Account, Windows: item.candidate.Windows, Threshold: settings.Threshold(item.candidate), Exempt: item.candidate.CutoffExempt})
	}
	choice := capacity.Select(input)
	chosen := viable[choice.Index]
	considered := make([]model.CapacityEvaluation, len(choice.Evaluations))
	for i, evaluation := range choice.Evaluations {
		considered[i] = model.CapacityEvaluation{Account: evaluation.Account, Model: viable[i].candidate.Model, Window: evaluation.Window, Threshold: evaluation.Threshold,
			CutoffExempt: evaluation.Exempt, Eligible: evaluation.Eligible, RecentLaunches: evaluation.RecentLaunches}
		if evaluation.UsageKnown {
			used := evaluation.UsedRatio
			considered[i].UsedRatio = &used
		}
	}
	if choice.Reason == capacity.ReasonAllOverThreshold {
		selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_all_over_threshold",
			Message: "every pool candidate is over its threshold, so the least used one will run", Details: map[string]any{"pool": selector.Pool, "account": chosen.candidate.Account}})
	}
	if status == capacity.StatusOK && !choice.Evaluations[choice.Index].UsageKnown {
		selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_account_unobserved",
			Message: "the chosen account has no usable usage in the capacity facts", Details: map[string]any{"pool": selector.Pool, "account": chosen.candidate.Account}})
	}
	selection.resolution = chosen.resolution
	selection.executable = chosen.candidate.Executable
	selection.capacity = &model.DelegationCapacity{Pool: selector.Pool, Mode: mode, Account: chosen.candidate.Account, Reason: choice.Reason, FactsStatus: status, FactsAgeSeconds: age, Considered: considered}
	return selection, nil
}

// resolvePoolCandidate keeps agent_preferences the one allowlist, so a pool can
// only choose among models a person could already name.
func resolvePoolCandidate(candidate config.CapacityCandidate, entries []delegation.Entry) (delegation.Resolution, error) {
	harness, err := launchrecipe.CanonicalHarness(candidate.Harness)
	if err != nil {
		return delegation.Resolution{}, err
	}
	selector := delegation.Selector{Harness: harness, Model: candidate.Model}
	if candidate.Effort != "" {
		selector.Settings = &delegation.Settings{Effort: candidate.Effort}
	}
	resolution, err := delegation.Resolve(selector, entries)
	if err != nil {
		return delegation.Resolution{}, err
	}
	// The caller never named this effort. Recording it as defaulted lets a
	// follow-up turn keep it rather than reading the provider's current profile.
	if candidate.Effort != "" {
		resolution.Defaulted = append(resolution.Defaulted, "effort")
	}
	return resolution, nil
}

// recentPoolLaunches counts this journal's pool launches per account. Running
// work always counts, and finished work counts for as long as facts may lag.
func (a *app) recentPoolLaunches(ctx context.Context, c common, now time.Time, window time.Duration) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(ctx, recentLaunchScanLimit)
	defer cancel()
	path, err := a.journalPath(c)
	if err != nil {
		return nil, err
	}
	journal, err := openJournalWithRetryContext(ctx, path, store.Options{ReadOnly: true})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer journal.Close()
	executions, err := journal.ListExecutions(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("list executions: %w", err)
	}
	counts := map[string]int{}
	for _, execution := range executions {
		if execution.Delegation == nil || execution.Delegation.Capacity == nil {
			continue
		}
		if !execution.State.Terminal() || now.Sub(execution.CreatedAt) <= window {
			counts[execution.Delegation.Capacity.Account]++
		}
	}
	return counts, nil
}

// pooledJournalSelector stores the resolved harness and model in place of the
// pool. An older binary decodes this record strictly and would read a pool-only
// selector as corrupt. The pool stays in the capacity record, and the request
// digest still covers the original.
func pooledJournalSelector(requested delegation.Selector, selected delegation.Entry) delegation.Selector {
	journaled := delegation.Selector{Harness: selected.Harness, Model: selected.Model, Host: requested.Host}
	if requested.Settings != nil {
		settings := *requested.Settings
		journaled.Settings = &settings
	}
	return journaled
}
