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

type recentOptions struct {
	limit        int
	state        string
	adapter      string
	labels       []string
	unreconciled bool
}

type recentExecution struct {
	ID              ids.ExecutionID `json:"id"`
	Labels          []string        `json:"labels"`
	Authority       model.Authority `json:"authority"`
	Adapter         string          `json:"adapter"`
	Mode            model.Mode      `json:"mode"`
	State           model.State     `json:"state"`
	Liveness        model.Liveness  `json:"liveness"`
	CreatedAt       time.Time       `json:"created_at"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	UpdatedAt       time.Time       `json:"updated_at"`
	TerminalAt      *time.Time      `json:"terminal_at,omitempty"`
	DurationSeconds float64         `json:"duration_seconds"`
	Unreconciled    bool            `json:"unreconciled"`
	AcknowledgedAt  *time.Time      `json:"acknowledged_at,omitempty"`
}

func (a *app) recent(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	opts, problem := parseRecent(args)
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
		return mapStoreError("list recent executions", err)
	}
	acks, err := journal.AcknowledgementIndex(ctx)
	if err != nil {
		return mapStoreError("list execution acknowledgements", err)
	}
	items := make([]recentExecution, 0, opts.limit)
	matched := 0
	for i := len(executions) - 1; i >= 0; i-- {
		execution := executions[i]
		if !recentMatches(execution, opts, acks) {
			continue
		}
		matched++
		if len(items) >= opts.limit {
			continue
		}
		items = append(items, projectRecent(execution, a.now().UTC(), acks))
	}
	lines := make([]output.Line, 0, len(items))
	for _, item := range items {
		fields := []output.Field{{Name: "state", Value: item.State}, {Name: "adapter", Value: item.Adapter}, {Name: "liveness", Value: item.Liveness}, {Name: "duration", Value: time.Duration(item.DurationSeconds * float64(time.Second)).Round(time.Second)}}
		if item.Unreconciled {
			fields = append(fields, output.Field{Name: "unreconciled", Value: true})
		}
		if len(item.Labels) != 0 {
			fields = append(fields, output.Field{Name: "labels", Value: item.Labels})
		}
		lines = append(lines, output.Line{Lead: item.ID.String(), Fields: fields})
	}
	result := map[string]any{"executions": items, "count": len(items), "has_more": matched > len(items), "host_local": true}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write recent executions", false, err)
	}
	return nil
}

func parseRecent(args []string) (recentOptions, *output.Error) {
	opts := recentOptions{limit: 20}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--unreconciled":
			opts.unreconciled = true
			continue
		}
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
		case "--state":
			if !validRecentState(value) {
				return opts, output.NewError(output.CodeUsage, "--state must be terminal, nonterminal, or an execution state", false).WithDetail("state", value)
			}
			opts.state = value
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
			return opts, output.NewError(output.CodeUsage, "unknown recent flag", false).WithDetail("flag", flag)
		}
	}
	if opts.unreconciled && opts.state == "nonterminal" {
		return opts, output.NewError(output.CodeUsage, "--unreconciled cannot be combined with --state nonterminal", false)
	}
	return opts, nil
}

func validRecentState(value string) bool {
	switch value {
	case "terminal", "nonterminal", string(model.StateCreated), string(model.StateStarting), string(model.StateRunning), string(model.StateWaiting), string(model.StateAttention), string(model.StateCompleted), string(model.StateFailed), string(model.StateCancelled), string(model.StateOrphaned):
		return true
	default:
		return false
	}
}

func recentMatches(execution model.Execution, opts recentOptions, acks store.AcknowledgementIndex) bool {
	if opts.adapter != "" && execution.Adapter != opts.adapter {
		return false
	}
	switch opts.state {
	case "terminal":
		if !execution.State.Terminal() {
			return false
		}
	case "nonterminal":
		if execution.State.Terminal() {
			return false
		}
	case "":
	default:
		if string(execution.State) != opts.state {
			return false
		}
	}
	for _, wanted := range opts.labels {
		if !containsArg(execution.Labels, wanted) {
			return false
		}
	}
	if opts.unreconciled && !acks.Unreconciled(execution) {
		return false
	}
	return true
}

func projectRecent(execution model.Execution, now time.Time, acks store.AcknowledgementIndex) recentExecution {
	start := execution.CreatedAt
	if execution.StartedAt != nil {
		start = *execution.StartedAt
	}
	end := now
	if execution.TerminalAt != nil {
		end = *execution.TerminalAt
	}
	duration := end.Sub(start)
	if duration < 0 {
		duration = 0
	}
	labels := append([]string(nil), execution.Labels...)
	if labels == nil {
		labels = []string{}
	}
	item := recentExecution{ID: execution.ID, Labels: labels, Authority: execution.Authority, Adapter: execution.Adapter, Mode: execution.Mode, State: execution.State, Liveness: execution.Liveness, CreatedAt: execution.CreatedAt, StartedAt: execution.StartedAt, UpdatedAt: execution.UpdatedAt, TerminalAt: execution.TerminalAt, DurationSeconds: duration.Seconds(), Unreconciled: acks.Unreconciled(execution)}
	if ack, ok := acks.ByID[execution.ID]; ok {
		acknowledgedAt := ack.AcknowledgedAt
		item.AcknowledgedAt = &acknowledgedAt
	}
	return item
}
