package main

import (
	"context"
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
	Reasons               []inboxReason   `json:"reasons"`
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
	for i := len(executions) - 1; i >= 0; i-- {
		execution := executions[i]
		if !inboxFilterMatches(execution, opts) {
			continue
		}
		item, actionable := projectInbox(execution, now, opts.staleAfter, acks)
		if !actionable {
			continue
		}
		matched++
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
	result := map[string]any{
		"executions":          items,
		"count":               len(items),
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

func projectInbox(execution model.Execution, now time.Time, staleAfter time.Duration, acks store.AcknowledgementIndex) (inboxExecution, bool) {
	observationAge := now.Sub(execution.Observation.ObservedAt)
	if observationAge < 0 {
		observationAge = 0
	}
	unreconciled := acks.Unreconciled(execution)
	reasons := make([]inboxReason, 0, 4)
	workHealth := "active"
	integrityConflicted := execution.Observation.Integrity == model.IntegrityConflicted
	if integrityConflicted {
		reasons = append(reasons, inboxReason{Code: "observation_integrity_conflicted", Domain: "integrity", Summary: "normalized execution evidence conflicts; outcome-dependent commands remain unavailable until the authority is reconciled"})
	}
	if execution.State == model.StateAttention {
		workHealth = "attention_required"
		reasons = append(reasons, inboxReason{Code: "attention_required", Domain: "work", Summary: "the execution authority requires a decision or intervention"})
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
		reasons = append(reasons, inboxReason{Code: "result_unreconciled", Domain: "collection", Summary: "the terminal result has not been collected with result or await"})
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
		Unreconciled: unreconciled, Reasons: reasons,
	}
	return item, len(reasons) != 0
}
