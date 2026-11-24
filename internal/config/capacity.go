package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/Git-on-my-level/agentctl/internal/capacity"
	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
)

const (
	// DefaultSwitchThreshold is the used ratio at which a candidate stops
	// receiving new work, so an account is left before its limit is hit.
	DefaultSwitchThreshold = 0.95
	// DefaultCapacityMaxAgeSeconds is how long published facts stay usable.
	DefaultCapacityMaxAgeSeconds = 3600
	// DefaultLaunchDebit is the used ratio one recent launch is assumed to cost
	// while work is running or its completion is newer than a usage observation,
	// so a fan-out spreads while facts lag without counting measured work twice.
	DefaultLaunchDebit = 0.02
	// DefaultCapacityEpsilon is the room reserved below the plan limit.
	DefaultCapacityEpsilon = 0.03
	// DefaultExpectedCost is the allowance share budgeted for the next run.
	DefaultExpectedCost = 0.01

	maxCapacityPools      = 32
	maxCapacityCandidates = 16
	maxCapacityWindows    = 8
)

var capacityNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Capacity lets delegate choose an account from usage facts another tool
// publishes. agentctl only reads the file and never measures usage.
type Capacity struct {
	File            string                  `json:"file,omitempty"`
	MaxAgeSeconds   int                     `json:"max_age_seconds,omitempty"`
	SwitchThreshold *float64                `json:"switch_threshold,omitempty"`
	LaunchDebit     *float64                `json:"launch_debit,omitempty"`
	Policy          capacity.Policy         `json:"policy,omitempty"`
	Epsilon         *float64                `json:"epsilon,omitempty"`
	ExpectedCost    *float64                `json:"expected_cost,omitempty"`
	Pools           map[string]CapacityPool `json:"pools,omitempty"`
}

// CapacityPool is a candidate list. It is not a role: agentctl gives the name no
// meaning, and callers choose a pool as they would a model.
type CapacityPool struct {
	Policy     capacity.Policy     `json:"policy,omitempty"`
	Candidates []CapacityCandidate `json:"candidates"`
}

// CapacityCandidate names one model on one account. The model must also resolve
// through agent_preferences, so a pool can't widen the reviewed allowlist.
// Executable selects the account's launcher; agentctl stores no credentials.
type CapacityCandidate struct {
	Harness         string   `json:"harness"`
	Model           string   `json:"model"`
	Effort          string   `json:"effort,omitempty"`
	Account         string   `json:"account"`
	Executable      string   `json:"executable,omitempty"`
	SwitchThreshold *float64 `json:"switch_threshold,omitempty"`
	CutoffExempt    bool     `json:"cutoff_exempt,omitempty"`
	Windows         []string `json:"windows,omitempty"`
}

// Threshold returns the candidate's override or the profile's value or the default.
func (c Capacity) Threshold(candidate CapacityCandidate) float64 {
	if candidate.SwitchThreshold != nil {
		return *candidate.SwitchThreshold
	}
	if c.SwitchThreshold != nil {
		return *c.SwitchThreshold
	}
	return DefaultSwitchThreshold
}

// MaxAge returns the configured facts age limit in seconds.
func (c Capacity) MaxAge() int {
	if c.MaxAgeSeconds > 0 {
		return c.MaxAgeSeconds
	}
	return DefaultCapacityMaxAgeSeconds
}

// Debit returns the configured per-launch ranking cost.
func (c Capacity) Debit() float64 {
	if c.LaunchDebit != nil {
		return *c.LaunchDebit
	}
	return DefaultLaunchDebit
}

// PoolPolicy returns the pool's override or the profile policy.
func (c Capacity) PoolPolicy(pool CapacityPool) capacity.Policy {
	if pool.Policy != "" {
		return pool.Policy
	}
	return c.Policy
}

// DrainBudget returns the reserve and estimated cost for the next launch.
func (c Capacity) DrainBudget() (epsilon, expectedCost float64) {
	epsilon, expectedCost = DefaultCapacityEpsilon, DefaultExpectedCost
	if c.Epsilon != nil {
		epsilon = *c.Epsilon
	}
	if c.ExpectedCost != nil {
		expectedCost = *c.ExpectedCost
	}
	return epsilon, expectedCost
}

// Validate checks shape only. Whether a candidate's model is allowed, and
// whether its executable exists, are host facts reported by config doctor.
func (c Capacity) Validate() error {
	if c.File != "" && !filepath.IsAbs(c.File) {
		return errors.New("capacity.file must be an absolute path")
	}
	if c.MaxAgeSeconds < 0 {
		return errors.New("capacity.max_age_seconds must not be negative")
	}
	if c.SwitchThreshold != nil && !validThreshold(*c.SwitchThreshold) {
		return errors.New("capacity.switch_threshold must be above 0 and at most 1")
	}
	if c.LaunchDebit != nil && (*c.LaunchDebit < 0 || *c.LaunchDebit > 1) {
		return errors.New("capacity.launch_debit must be between 0 and 1")
	}
	if !validCapacityPolicy(c.Policy) {
		return errors.New("capacity.policy must be drain_before_reset or soonest_reset")
	}
	if c.Epsilon != nil && !validCapacityRatio(*c.Epsilon) {
		return errors.New("capacity.epsilon must be between 0 and 1")
	}
	if c.ExpectedCost != nil && !validCapacityRatio(*c.ExpectedCost) {
		return errors.New("capacity.expected_cost must be between 0 and 1")
	}
	if len(c.Pools) > maxCapacityPools {
		return fmt.Errorf("capacity.pools allows at most %d pools", maxCapacityPools)
	}
	for name, pool := range c.Pools {
		if !capacityNamePattern.MatchString(name) {
			return fmt.Errorf("capacity pool name %q must be lowercase letters, digits, dots, dashes or underscores", name)
		}
		if err := pool.validate(name); err != nil {
			return err
		}
	}
	return nil
}

func (p CapacityPool) validate(name string) error {
	if !validCapacityPolicy(p.Policy) {
		return fmt.Errorf("capacity pool %q policy must be drain_before_reset or soonest_reset", name)
	}
	if len(p.Candidates) == 0 || len(p.Candidates) > maxCapacityCandidates {
		return fmt.Errorf("capacity pool %q needs 1 to %d candidates", name, maxCapacityCandidates)
	}
	seen := make(map[string]struct{}, len(p.Candidates))
	for i, candidate := range p.Candidates {
		where := fmt.Sprintf("capacity pool %q candidate %d", name, i)
		harness, err := launchrecipe.CanonicalHarness(candidate.Harness)
		if err != nil {
			return fmt.Errorf("%s: unknown harness", where)
		}
		if strings.TrimSpace(candidate.Model) == "" || strings.IndexFunc(candidate.Model, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s: model is required", where)
		}
		if !capacityNamePattern.MatchString(candidate.Account) {
			return fmt.Errorf("%s: account must be lowercase letters, digits, dots, dashes or underscores", where)
		}
		if candidate.Executable != "" && !filepath.IsAbs(candidate.Executable) {
			return fmt.Errorf("%s: executable must be an absolute path", where)
		}
		if candidate.SwitchThreshold != nil && !validThreshold(*candidate.SwitchThreshold) {
			return fmt.Errorf("%s: switch_threshold must be above 0 and at most 1", where)
		}
		if candidate.CutoffExempt && candidate.SwitchThreshold != nil {
			return fmt.Errorf("%s: cutoff_exempt and switch_threshold conflict", where)
		}
		if len(candidate.Windows) > maxCapacityWindows {
			return fmt.Errorf("%s: at most %d windows", where, maxCapacityWindows)
		}
		for _, window := range candidate.Windows {
			if strings.TrimSpace(window) == "" || strings.IndexFunc(window, unicode.IsControl) >= 0 {
				return fmt.Errorf("%s: window names must be nonempty", where)
			}
		}
		key := strings.Join([]string{harness, candidate.Model, candidate.Effort, candidate.Account}, "\x00")
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%s: repeats an earlier candidate", where)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validThreshold(value float64) bool {
	return value > 0 && value <= 1
}

func validCapacityPolicy(policy capacity.Policy) bool {
	return policy == "" || policy == capacity.ModeDrainBeforeReset || policy == capacity.ModeSoonestReset
}

func validCapacityRatio(value float64) bool {
	return value >= 0 && value <= 1
}

func cloneCapacity(in *Capacity) *Capacity {
	if in == nil {
		return nil
	}
	out := *in
	out.SwitchThreshold = cloneFloat(in.SwitchThreshold)
	out.LaunchDebit = cloneFloat(in.LaunchDebit)
	out.Epsilon = cloneFloat(in.Epsilon)
	out.ExpectedCost = cloneFloat(in.ExpectedCost)
	if in.Pools != nil {
		out.Pools = make(map[string]CapacityPool, len(in.Pools))
		for name, pool := range in.Pools {
			candidates := make([]CapacityCandidate, len(pool.Candidates))
			for i, candidate := range pool.Candidates {
				candidate.SwitchThreshold = cloneFloat(candidate.SwitchThreshold)
				candidate.Windows = append([]string(nil), candidate.Windows...)
				candidates[i] = candidate
			}
			pool.Candidates = candidates
			out.Pools[name] = pool
		}
	}
	return &out
}

func cloneFloat(in *float64) *float64 {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}

// addCapacityChecks reports host facts validation can't see: each candidate's
// executable, its allowlist entry and its recipe. It runs nothing and reads no
// facts file, so a missing producer doesn't fail doctor.
func addCapacityChecks(report *ProvenanceReport, profile Profile, opts ProvenanceOptions) {
	if profile.Capacity == nil {
		return
	}
	names := make([]string, 0, len(profile.Capacity.Pools))
	for name := range profile.Capacity.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for i, candidate := range profile.Capacity.Pools[name].Candidates {
			prefix := fmt.Sprintf("capacity.pool.%s.candidate.%d", name, i)
			if candidate.Executable != "" {
				entry := inspectExecutable(candidate.Executable, opts)
				ok := entry.Resolvable && entry.DigestOK
				report.addCheck(ProvenanceCheck{Name: prefix + ".executable", OK: ok, Status: status(ok), Value: entry.Resolved, Detail: entry.Error})
			}
			allowed := false
			if profile.AgentPreferences != nil {
				for _, preference := range profile.AgentPreferences.Preferred {
					want, wantErr := launchrecipe.CanonicalHarness(candidate.Harness)
					got, gotErr := launchrecipe.CanonicalHarness(preference.Agent)
					if wantErr == nil && gotErr == nil && want == got && preference.Model == candidate.Model {
						allowed = true
						break
					}
				}
			}
			detail := ""
			if !allowed {
				detail = "no agent_preferences entry has this harness and model"
			}
			report.addCheck(ProvenanceCheck{Name: prefix + ".model", OK: allowed, Status: status(allowed), Value: candidate.Model, Detail: detail})
		}
	}
}
