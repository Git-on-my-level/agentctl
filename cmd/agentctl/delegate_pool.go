package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/capacity"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/delegation"
	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// recentLaunchScanLimit bounds the journal read a pool does before launching.
const recentLaunchScanLimit = 3 * time.Second

// poolSelection is one pool's outcome. The zero value means no pool was named
// or inferred, so a request naming a family or model takes the default account.
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

// selectDelegatePool picks one account from published facts. Unusable facts or
// an unreadable journal send drain selection through the legacy policy.
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
	policy := settings.PoolPolicy(pool)
	drain := policy != capacity.ModeSoonestReset
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
	if drain && status != capacity.StatusOK {
		selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_facts_unusable",
			Message: "capacity facts are unusable, so the pool used soonest_reset", Details: map[string]any{"pool": selector.Pool, "facts_status": status}})
	} else if status == capacity.StatusMissing || status == capacity.StatusStale || status == capacity.StatusInvalid {
		selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_facts_unusable",
			Message: "capacity facts are unusable, so the pool used its configured order", Details: map[string]any{"pool": selector.Pool, "facts_status": status}})
	}
	recent, recentProblem := a.recentPoolLaunches(ctx, c, now, maxAge)
	if recentProblem != nil {
		message := "recent launches could not be read, so the choice ignored them"
		if drain {
			policy = capacity.ModeSoonestReset
			message = "recent launches could not be read, so the pool used soonest_reset without launch debits"
		}
		selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_recent_launches_unread",
			Message: message, Details: map[string]any{"pool": selector.Pool}})
	}

	epsilon, expectedCost := settings.DrainBudget()
	input := capacity.Input{Facts: facts, Launches: recent, LaunchDebit: settings.Debit(), MaxAge: maxAge, Now: now,
		Policy: policy, Epsilon: epsilon, ExpectedCost: expectedCost}
	for _, item := range viable {
		input.Candidates = append(input.Candidates, capacity.Candidate{Account: item.candidate.Account, Windows: item.candidate.Windows, Threshold: settings.Threshold(item.candidate), Exempt: item.candidate.CutoffExempt})
	}
	choice := capacity.Select(input)
	if policy != capacity.ModeSoonestReset && facts != nil && choice.Mode == capacity.ModeSoonestReset {
		selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_window_unusable",
			Message: "a capacity window observation is unusable, so the pool used soonest_reset", Details: map[string]any{"pool": selector.Pool}})
	}
	chosen := viable[choice.Index]
	considered := make([]model.CapacityEvaluation, len(choice.Evaluations))
	for i, evaluation := range choice.Evaluations {
		candidate := viable[i].candidate
		if choice.Mode == capacity.ModeDrainBeforeReset {
			var fields []string
			if settings.SwitchThreshold != nil || candidate.SwitchThreshold != nil {
				fields = append(fields, "switch_threshold")
			}
			if candidate.CutoffExempt {
				fields = append(fields, "cutoff_exempt")
			}
			if len(candidate.Windows) > 0 {
				fields = append(fields, "windows")
			}
			if len(fields) > 0 {
				selection.warnings = append(selection.warnings, output.Warning{Code: "capacity_policy_overrides_limits",
					Message: "configured capacity limits apply only with soonest_reset", Details: map[string]any{"pool": selector.Pool, "account": candidate.Account, "fields": fields}})
			}
		}
		considered[i] = model.CapacityEvaluation{Account: evaluation.Account, Model: candidate.Model, Window: evaluation.Window, Threshold: evaluation.Threshold,
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
	selection.capacity = &model.DelegationCapacity{Pool: selector.Pool, Mode: choice.Mode, Account: chosen.candidate.Account, Reason: choice.Reason, FactsStatus: status, FactsAgeSeconds: age, Considered: considered}
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

// recentPoolLaunches reads local pool work. Selection compares completion times
// to each window's observation, so usage already measured is not debited again.
// Running work always reserves room; long-running work is retained by completion
// time rather than creation time.
func (a *app) recentPoolLaunches(ctx context.Context, c common, now time.Time, window time.Duration) ([]capacity.Launch, error) {
	ctx, cancel := context.WithTimeout(ctx, recentLaunchScanLimit)
	defer cancel()
	path, err := a.journalPath(c)
	if err != nil {
		return nil, err
	}
	journal, err := openJournalWithBusyBudget(ctx, path, store.Options{ReadOnly: true}, true, c.journalBusyBudget)
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
	var launches []capacity.Launch
	for _, execution := range executions {
		if execution.RemoteForwarded() || execution.Delegation == nil || execution.Delegation.Capacity == nil {
			continue
		}
		if execution.TerminalAt == nil || !execution.TerminalAt.Before(now.Add(-window)) {
			launches = append(launches, capacity.Launch{Account: execution.Delegation.Capacity.Account, TerminalAt: execution.TerminalAt})
		}
	}
	return launches, nil
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

// poolInference is the outcome of matching a family or model request to a
// pool. An empty pool means the request keeps its catalog resolution.
type poolInference struct {
	pool     string
	from     string
	warnings []output.Warning
}

// inferDelegatePool finds the one pool whose candidates all run the harness and
// model a family or model request resolved to, so the account is chosen by
// capacity just as if selector.pool had been given. No match changes nothing.
// A match it cannot honour exactly, because several pools hold the model, the
// pool mixes models, or the request pins a setting a candidate decides
// differently, keeps the default account and warns rather than guessing.
func inferDelegatePool(profile config.Profile, selector delegation.Selector, resolution delegation.Resolution) poolInference {
	if profile.Capacity == nil || len(profile.Capacity.Pools) == 0 {
		return poolInference{}
	}
	got := resolution.Resolved
	var matched []string
	for name, pool := range profile.Capacity.Pools {
		for _, candidate := range pool.Candidates {
			if poolCandidateRuns(candidate, got) {
				matched = append(matched, name)
				break
			}
		}
	}
	if len(matched) == 0 {
		return poolInference{}
	}
	sort.Strings(matched)
	from := model.CapacityInferredFromFamily
	if strings.TrimSpace(selector.Model) != "" {
		from = model.CapacityInferredFromModel
	}
	details := map[string]any{"harness": got.Harness, "model": got.Model, "pools": matched, "inferred_from": from}
	if len(matched) > 1 {
		return poolInference{warnings: []output.Warning{{Code: "capacity_pool_inference_ambiguous",
			Message: "the resolved model is in more than one capacity pool, so the default account will run; name selector.pool to choose an account by capacity", Details: details}}}
	}
	name := matched[0]
	conflict := func(reason, field string) poolInference {
		details["pool"] = name
		details["reason"] = reason
		if field != "" {
			details["field"] = field
		}
		return poolInference{warnings: []output.Warning{{Code: "capacity_pool_inference_conflict",
			Message: "the resolved model is in a capacity pool that cannot honour this request exactly, so the default account will run", Details: details}}}
	}
	var pinned delegation.Settings
	if selector.Settings != nil {
		pinned = *selector.Settings
	}
	entries := delegateEntries(profile)
	viable := 0
	for _, candidate := range profile.Capacity.Pools[name].Candidates {
		if !poolCandidateRuns(candidate, got) {
			return conflict("pool_mixes_models", "model")
		}
		candidateResolution, err := resolvePoolCandidate(candidate, entries)
		if err != nil {
			// Pool selection skips it with capacity_candidate_unresolved.
			continue
		}
		viable++
		chosen := candidateResolution.Resolved
		for _, check := range []struct{ field, want, have, gotValue string }{
			{"settings.effort", pinned.Effort, chosen.Effort, got.Effort},
			{"settings.speed", pinned.Speed, chosen.Speed, got.Speed},
			{"family", selector.Family, chosen.Family, got.Family},
			{"version", selector.Version, chosen.Version, got.Version},
		} {
			if strings.TrimSpace(check.want) != "" && !strings.EqualFold(strings.TrimSpace(check.have), strings.TrimSpace(check.gotValue)) {
				return conflict("pinned_setting", check.field)
			}
		}
	}
	if viable == 0 {
		return conflict("no_resolvable_candidate", "")
	}
	return poolInference{pool: name, from: from}
}

func poolCandidateRuns(candidate config.CapacityCandidate, entry delegation.Entry) bool {
	harness, err := launchrecipe.CanonicalHarness(candidate.Harness)
	return err == nil && harness == entry.Harness && strings.TrimSpace(candidate.Model) == entry.Model
}

// inferredPoolDefaulted reports what the caller left out. A pool candidate's
// own resolution starts from harness and model, so it never marks those, and a
// field the caller pinned stays caller-supplied.
func inferredPoolDefaulted(requested delegation.Resolution, chosen delegation.Resolution) []string {
	pinned := map[string]bool{}
	for _, field := range []struct {
		name  string
		value string
	}{{"harness", requested.Requested.Harness}, {"family", requested.Requested.Family}, {"version", requested.Requested.Version}, {"model", requested.Requested.Model}} {
		pinned[field.name] = strings.TrimSpace(field.value) != ""
	}
	if settings := requested.Requested.Settings; settings != nil {
		pinned["speed"] = strings.TrimSpace(settings.Speed) != ""
		pinned["effort"] = strings.TrimSpace(settings.Effort) != ""
	}
	fields := append([]string(nil), requested.Defaulted...)
	seen := map[string]bool{}
	for _, field := range fields {
		seen[field] = true
	}
	for _, field := range chosen.Defaulted {
		if !seen[field] && !pinned[field] {
			fields = append(fields, field)
			seen[field] = true
		}
	}
	return fields
}
