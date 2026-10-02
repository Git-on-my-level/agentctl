package main

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const (
	defaultInboxStaleAfter = time.Hour
	minimumInboxStaleAfter = time.Minute
	maximumInboxStaleAfter = 30 * 24 * time.Hour
)

type inboxOptions struct {
	limit      int
	staleAfter time.Duration
	adapter    string
	labels     []string
}

type inboxReason struct {
	Code       string   `json:"code"`
	Domain     string   `json:"domain"`
	Summary    string   `json:"summary"`
	AgeSeconds *float64 `json:"age_seconds,omitempty"`
}

type inboxExecution struct {
	ID                    ids.ExecutionID `json:"id"`
	Labels                []string        `json:"labels"`
	Authority             model.Authority `json:"authority"`
	Adapter               string          `json:"adapter"`
	Mode                  model.Mode      `json:"mode"`
	State                 model.State     `json:"state"`
	Liveness              model.Liveness  `json:"liveness"`
	WorkHealth            string          `json:"work_health"`
	ToolHealth            string          `json:"tool_health"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
	ObservationAgeSeconds float64         `json:"observation_age_seconds"`
	Unreconciled          bool            `json:"unreconciled"`
	RenotifyAgeTier       string          `json:"renotify_age_tier,omitempty"`
	RenotifyAgeSeconds    *float64        `json:"renotify_age_seconds,omitempty"`
	Reasons               []inboxReason   `json:"reasons"`
	// NextActions is per item because inbox reports many executions at once and
	// a document-level action cannot name which one it applies to.
	NextActions []output.NextAction `json:"next_actions"`
}

// Re-notify age tiers are a pure function of time since the terminal event, so
// the nag ladder is deterministic and needs no daemon or timer. An
// unacknowledged terminal never leaves the inbox on its own: it only climbs
// tiers. Only a recorded acknowledgement removes it, and that stamp is a
// deliberate, final act (see docs/agent-ergonomics.md).
var renotifyTiers = []struct {
	name string
	age  time.Duration
}{
	{"fresh", 24 * time.Hour},
	{"aging", 72 * time.Hour},
}

// renotifyTierFor reports the re-notify ladder position for one unacknowledged
// terminal: fresh under a day, aging under three days, persistent beyond.
func renotifyTierFor(age time.Duration) string {
	for _, tier := range renotifyTiers {
		if age < tier.age {
			return tier.name
		}
	}
	return "persistent"
}

func (a *app) inbox(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	opts, problem := parseInbox(args)
	if problem != nil {
		return problem
	}
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	executions, err := journal.ListExecutions(ctx, false)
	if err != nil {
		return mapStoreError("list inbox executions", err)
	}
	acks, err := journal.AcknowledgementIndex(ctx)
	if err != nil {
		return mapStoreError("list execution acknowledgements", err)
	}
	now := a.now().UTC()
	items := make([]inboxExecution, 0, opts.limit)
	matched := 0
	// Re-notify ordering: unacknowledged terminals come first, oldest terminal
	// age first, so an old uncollected result can never be buried under newer
	// activity. The remainder keeps the newest-first recency order.
	unacked, ordered := make([]inboxExecution, 0, opts.limit), make([]inboxExecution, 0, opts.limit)
	for i := len(executions) - 1; i >= 0; i-- {
		execution := executions[i]
		if !inboxFilterMatches(execution, opts) {
			continue
		}
		item, actionable := projectInbox(execution, now, opts.staleAfter, acks, renderer.Mode)
		if !actionable {
			continue
		}
		if item.Unreconciled && item.RenotifyAgeSeconds != nil {
			unacked = append(unacked, item)
		} else {
			ordered = append(ordered, item)
		}
		matched++
	}
	// count is the returned projection; total is the full actionable set, so a
	// caller can size the backlog without paging to discover it. Unacknowledged
	// terminals lead the page, oldest terminal age first, so persistent
	// re-notification survives the default limit; the rest keeps newest-first
	// recency order. Both windows are capped at limit, so cost stays bounded.
	sort.SliceStable(unacked, func(a, b int) bool {
		return *unacked[a].RenotifyAgeSeconds > *unacked[b].RenotifyAgeSeconds
	})
	items = items[:0]
	for _, item := range unacked {
		if len(items) < opts.limit {
			items = append(items, item)
		}
	}
	for _, item := range ordered {
		if len(items) < opts.limit {
			items = append(items, item)
		}
	}
	lines := make([]output.Line, 0, len(items))
	for _, item := range items {
		codes := make([]string, 0, len(item.Reasons))
		for _, reason := range item.Reasons {
			codes = append(codes, reason.Code)
		}
		fields := []output.Field{
			{Name: "state", Value: item.State},
			{Name: "work", Value: item.WorkHealth},
			{Name: "tool", Value: item.ToolHealth},
			{Name: "why", Value: codes},
			{Name: "observed_ago", Value: time.Duration(item.ObservationAgeSeconds * float64(time.Second)).Round(time.Second)},
		}
		if len(item.Labels) != 0 {
			fields = append(fields, output.Field{Name: "labels", Value: item.Labels})
		}
		lines = append(lines, output.Line{Lead: item.ID.String(), Fields: fields})
	}
	// count is the returned projection; total is the full actionable set, so a
	// caller can size the backlog without paging to discover it.
	result := map[string]any{
		"executions":          items,
		"count":               len(items),
		"total":               matched,
		"has_more":            matched > len(items),
		"host_local":          true,
		"as_of":               now,
		"stale_after_seconds": opts.staleAfter.Seconds(),
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write inbox", false, err)
	}
	return nil
}

func parseInbox(args []string) (inboxOptions, *output.Error) {
	opts := inboxOptions{limit: 20, staleAfter: defaultInboxStaleAfter}
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return opts, output.NewError(output.CodeUsage, args[i]+" requires a value", false)
		}
		flag, value := args[i], strings.TrimSpace(args[i+1])
		i++
		switch flag {
		case "--limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 200 {
				return opts, output.NewError(output.CodeUsage, "--limit must be between 1 and 200", false)
			}
			opts.limit = limit
		case "--stale-after":
			staleAfter, err := time.ParseDuration(value)
			if err != nil || staleAfter < minimumInboxStaleAfter || staleAfter > maximumInboxStaleAfter {
				return opts, output.NewError(output.CodeUsage, "--stale-after must be a duration between 1m and 720h", false).WithDetail("stale_after", value)
			}
			opts.staleAfter = staleAfter
		case "--adapter":
			if value == "" {
				return opts, output.NewError(output.CodeUsage, "--adapter cannot be empty", false)
			}
			opts.adapter = value
		case "--label":
			if !validRunLabel(value) {
				return opts, output.NewError(output.CodeUsage, "--label must be an exact valid label", false).WithDetail("label", value)
			}
			opts.labels = append(opts.labels, value)
		default:
			return opts, output.NewError(output.CodeUsage, "unknown inbox flag", false).WithDetail("flag", flag)
		}
	}
	return opts, nil
}

func inboxFilterMatches(execution model.Execution, opts inboxOptions) bool {
	if opts.adapter != "" && execution.Adapter != opts.adapter {
		return false
	}
	for _, wanted := range opts.labels {
		if !containsArg(execution.Labels, wanted) {
			return false
		}
	}
	return true
}

func projectInbox(execution model.Execution, now time.Time, staleAfter time.Duration, acks store.AcknowledgementIndex, mode output.Mode) (inboxExecution, bool) {
	observationAge := now.Sub(execution.Observation.ObservedAt)
	if observationAge < 0 {
		observationAge = 0
	}
	unreconciled := acks.Unreconciled(execution)
	// Re-notify age is time since the terminal event, independent of the
	// observation age used for stale-running detection.
	var renotifyAgeSeconds *float64
	if unreconciled && execution.TerminalAt != nil {
		age := now.Sub(*execution.TerminalAt)
		if age < 0 {
			age = 0
		}
		seconds := age.Seconds()
		renotifyAgeSeconds = &seconds
	}
	reasons := make([]inboxReason, 0, 4)
	workHealth := "active"
	integrityConflicted := execution.Observation.Integrity == model.IntegrityConflicted
	if integrityConflicted {
		reasons = append(reasons, inboxReason{Code: "observation_integrity_conflicted", Domain: "integrity", Summary: "normalized execution evidence conflicts; outcome-dependent commands remain unavailable until the authority is reconciled"})
	}
	actions := []output.NextAction{}
	if execution.State == model.StateAttention {
		workHealth = "attention_required"
		reasons = append(reasons, inboxReason{Code: "attention_required", Domain: "work", Summary: "the execution authority requires a decision or intervention"})
		// Attention is the one inbox reason whose escape is not `result`: the
		// authority decides, and only then does a wait through attention end.
		actions = append(actions, attentionNextActions(mode, execution)...)
	}
	if unreconciled && (execution.State == model.StateFailed || execution.State == model.StateOrphaned) {
		if execution.State == model.StateFailed {
			workHealth = "failed"
			reasons = append(reasons, inboxReason{Code: "execution_failed", Domain: "work", Summary: "the terminal execution failed and its result has not been collected"})
		} else {
			workHealth = "orphaned"
			reasons = append(reasons, inboxReason{Code: "execution_orphaned", Domain: "work", Summary: "the terminal execution was orphaned and its result has not been collected"})
		}
	}
	if unreconciled {
		if workHealth == "active" {
			workHealth = "result_ready"
		}
		reasons = append(reasons, inboxReason{Code: "result_unreconciled", Domain: "collection", Summary: "the terminal result has not been collected with result or await", AgeSeconds: renotifyAgeSeconds})
	}
	if execution.State == model.StateRunning && observationAge >= staleAfter {
		age := observationAge.Seconds()
		workHealth = "observation_stale"
		reasons = append(reasons, inboxReason{Code: "running_observation_stale", Domain: "observation", Summary: "running work has no observation within the selected age bound", AgeSeconds: &age})
	}
	if !execution.State.Terminal() && execution.Liveness == model.LivenessUnreachable && observationAge >= staleAfter {
		age := observationAge.Seconds()
		reasons = append(reasons, inboxReason{Code: "tool_unreachable", Domain: "tool", Summary: "the runtime is unreachable; this does not prove the work failed", AgeSeconds: &age})
	}
	if integrityConflicted {
		workHealth = "integrity_conflicted"
	}
	labels := append([]string(nil), execution.Labels...)
	if labels == nil {
		labels = []string{}
	}
	item := inboxExecution{
		ID: execution.ID, Labels: labels, Authority: execution.Authority, Adapter: execution.Adapter, Mode: execution.Mode,
		State: execution.State, Liveness: execution.Liveness, WorkHealth: workHealth, ToolHealth: string(execution.Liveness),
		CreatedAt: execution.CreatedAt, UpdatedAt: execution.UpdatedAt, ObservationAgeSeconds: observationAge.Seconds(),
		Unreconciled: unreconciled, RenotifyAgeSeconds: renotifyAgeSeconds, Reasons: reasons, NextActions: actions,
	}
	// Unreconciled implies TerminalAt != nil, so the tier lookup is safe here.
	if unreconciled {
		item.RenotifyAgeTier = renotifyTierFor(now.Sub(*execution.TerminalAt))
	}
	return item, len(reasons) != 0
}
