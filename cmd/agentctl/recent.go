package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
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
	liveness     string
	adapter      string
	labels       []string
	unreconciled bool
	since        *time.Time
	until        *time.Time
	cursor       string
	summary      bool
	caller       string
}

type recentExecution struct {
	Caller                *model.ExecutionCaller `json:"caller,omitempty"`
	ID                    ids.ExecutionID        `json:"id"`
	Labels                []string               `json:"labels"`
	Authority             model.Authority        `json:"authority"`
	Adapter               string                 `json:"adapter"`
	Mode                  model.Mode             `json:"mode"`
	State                 model.State            `json:"state"`
	Liveness              model.Liveness         `json:"liveness"`
	CreatedAt             time.Time              `json:"created_at"`
	StartedAt             *time.Time             `json:"started_at,omitempty"`
	UpdatedAt             time.Time              `json:"updated_at"`
	TerminalAt            *time.Time             `json:"terminal_at,omitempty"`
	DurationSeconds       float64                `json:"duration_seconds"`
	Unreconciled          bool                   `json:"unreconciled"`
	AcknowledgedAt        *time.Time             `json:"acknowledged_at,omitempty"`
	AcknowledgementSource string                 `json:"acknowledgement_source,omitempty"`
}

func (a *app) recent(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	opts, problem := parseRecent(args)
	if problem != nil {
		return problem
	}
	page, problem := decodeRecentCursor(opts)
	if problem != nil {
		return problem
	}
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	host, err := journal.HostID(ctx)
	if err != nil {
		return mapStoreError("read journal identity", err)
	}
	if page != nil && page.Host != host.String() {
		return recentCursorError("cursor belongs to a different journal")
	}
	observedAt := a.now().UTC()
	asOf := observedAt
	if page != nil {
		asOf = page.AsOf
	}
	executions, err := journal.ListExecutions(ctx, false)
	if err != nil {
		return mapStoreError("list recent executions", err)
	}
	acks, err := journal.AcknowledgementIndex(ctx)
	if err != nil {
		return mapStoreError("list execution acknowledgements", err)
	}
	items := make([]recentExecution, 0, opts.limit)
	matched, remaining := 0, 0
	summary := recentUsageSummary{ByAdapter: map[string]int{}, ByAuthority: map[string]int{}, ByState: map[string]int{}, ByCaller: map[string]int{}}
	for i := len(executions) - 1; i >= 0; i-- {
		execution := executions[i]
		if execution.CreatedAt.After(asOf) || !recentMatches(execution, opts, acks) {
			continue
		}
		matched++
		summary.ByAdapter[execution.Adapter]++
		summary.ByAuthority[string(execution.Authority)]++
		summary.ByState[string(execution.State)]++
		caller := "unknown"
		if execution.Caller != nil {
			caller = string(execution.Caller.Harness)
		}
		summary.ByCaller[caller]++
		if acks.Unreconciled(execution) {
			summary.Unreconciled++
		}
		if page != nil && (execution.CreatedAt.After(page.CreatedAt) || (execution.CreatedAt.Equal(page.CreatedAt) && execution.ID.String() >= page.ID)) {
			continue
		}
		remaining++
		if !opts.summary && len(items) < opts.limit {
			items = append(items, projectRecent(execution, observedAt, acks))
		}
	}
	hasMore := !opts.summary && remaining > len(items)
	var next string
	if hasMore {
		last := items[len(items)-1]
		data, _ := json.Marshal(recentCursor{Version: 1, Host: host.String(), Filter: recentFilterDigest(opts), AsOf: asOf, CreatedAt: last.CreatedAt, ID: last.ID.String()})
		next = base64.RawURLEncoding.EncodeToString(data)
	}
	lines := make([]output.Line, 0, len(items))
	for _, item := range items {
		fields := []output.Field{{Name: "state", Value: item.State}, {Name: "adapter", Value: item.Adapter}, {Name: "liveness", Value: item.Liveness}, {Name: "duration", Value: time.Duration(item.DurationSeconds * float64(time.Second)).Round(time.Second)}}
		if item.Unreconciled {
			fields = append(fields, output.Field{Name: "unreconciled", Value: true})
		}
		if item.AcknowledgementSource != "" {
			fields = append(fields, output.Field{Name: "acknowledgement_source", Value: item.AcknowledgementSource})
		}
		if len(item.Labels) != 0 {
			fields = append(fields, output.Field{Name: "labels", Value: item.Labels})
		}
		if item.Caller != nil {
			fields = append(fields, output.Field{Name: "caller", Value: item.Caller.Harness})
		}
		lines = append(lines, output.Line{Lead: item.ID.String(), Fields: fields})
	}
	result := map[string]any{"schema_version": 1, "executions": items, "count": len(items), "total": matched, "has_more": hasMore, "host_local": true, "origin_host_id": host, "as_of": asOf, "state_consistency": "live_per_page", "coverage": "retained_executions_only", "preflight_failures": "not_recorded"}
	if next != "" {
		result["next_cursor"] = next
	}
	if opts.summary {
		result["summary"] = summary
		lines = append(lines, output.Line{Lead: "recent.summary", Fields: []output.Field{{Name: "total", Value: matched}, {Name: "unreconciled", Value: summary.Unreconciled}, {Name: "by_adapter", Value: summary.ByAdapter}, {Name: "by_authority", Value: summary.ByAuthority}, {Name: "by_state", Value: summary.ByState}, {Name: "by_caller", Value: summary.ByCaller}}})
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write recent executions", false, err)
	}
	return nil
}

type recentUsageSummary struct {
	ByAdapter    map[string]int `json:"by_adapter"`
	ByAuthority  map[string]int `json:"by_authority"`
	ByState      map[string]int `json:"by_state"`
	ByCaller     map[string]int `json:"by_caller"`
	Unreconciled int            `json:"unreconciled"`
}

// Cursors carry a creation boundary, not a persisted snapshot. State and
// acknowledgement filters are re-evaluated on each page. No write or result
// read is needed, and deleting the cursor row cannot strand pagination.
type recentCursor struct {
	Version   int       `json:"v"`
	Host      string    `json:"host"`
	Filter    string    `json:"filter"`
	AsOf      time.Time `json:"as_of"`
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func recentFilterDigest(o recentOptions) string {
	labels := append([]string{}, o.labels...)
	sort.Strings(labels)
	unique := labels[:0]
	for _, v := range labels {
		if len(unique) == 0 || unique[len(unique)-1] != v {
			unique = append(unique, v)
		}
	}
	data, _ := json.Marshal(struct {
		State, Liveness, Adapter, Caller string
		Labels                           []string
		Unreconciled                     bool
		Since, Until                     *time.Time
	}{o.state, o.liveness, o.adapter, o.caller, unique, o.unreconciled, o.since, o.until})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func recentCursorError(message string) *output.Error {
	return output.NewError(output.CodeUsage, message, false).WithDetail("diagnostic_code", "recent_invalid_cursor")
}
func decodeRecentCursor(o recentOptions) (*recentCursor, *output.Error) {
	if o.cursor == "" {
		return nil, nil
	}
	if len(o.cursor) > 2048 {
		return nil, recentCursorError("invalid recent cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(o.cursor)
	if err != nil {
		return nil, recentCursorError("invalid recent cursor")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cursor recentCursor
	if err := decoder.Decode(&cursor); err != nil {
		return nil, recentCursorError("invalid recent cursor")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, recentCursorError("invalid recent cursor")
	}
	if _, err := ids.ParseHostID(cursor.Host); err != nil {
		return nil, recentCursorError("invalid recent cursor")
	}
	if _, err := ids.ParseExecutionID(cursor.ID); err != nil {
		return nil, recentCursorError("invalid recent cursor")
	}
	if cursor.Version != 1 || cursor.AsOf.IsZero() || cursor.CreatedAt.IsZero() || cursor.CreatedAt.After(cursor.AsOf) {
		return nil, recentCursorError("invalid recent cursor")
	}
	if cursor.Filter != recentFilterDigest(o) {
		return nil, recentCursorError("cursor filters differ; retain the original filters or start a new export")
	}
	return &cursor, nil
}

func parseRecent(args []string) (recentOptions, *output.Error) {
	opts := recentOptions{limit: 20}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--summary":
			opts.summary = true
			continue
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
		case "--since", "--until":
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return opts, output.NewError(output.CodeUsage, flag+" must be an RFC3339 timestamp", false)
			}
			parsed = parsed.UTC()
			if flag == "--since" {
				opts.since = &parsed
			} else {
				opts.until = &parsed
			}
		case "--cursor":
			if value == "" {
				return opts, recentCursorError("cursor must not be empty")
			}
			opts.cursor = value
		case "--caller":
			if value != "unknown" && (&model.ExecutionCaller{Harness: model.CallerHarness(value), Provenance: model.CallerDeclared}).Validate() != nil {
				return opts, output.NewError(output.CodeUsage, "--caller must be a supported caller harness or unknown", false)
			}
			opts.caller = value
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
		case "--liveness":
			if !validRecentLiveness(value) {
				return opts, output.NewError(output.CodeUsage, "--liveness must be unknown, alive, blocked, exited, or unreachable", false).WithDetail("liveness", value)
			}
			opts.liveness = value
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
	if opts.summary && opts.cursor != "" {
		return opts, output.NewError(output.CodeUsage, "--summary cannot be combined with --cursor", false)
	}
	if opts.since != nil && opts.until != nil && !opts.since.Before(*opts.until) {
		return opts, output.NewError(output.CodeUsage, "--since must precede --until", false)
	}
	if opts.unreconciled && opts.state == "nonterminal" {
		return opts, output.NewError(output.CodeUsage, "--unreconciled cannot be combined with --state nonterminal", false)
	}
	return opts, nil
}

func validRecentLiveness(value string) bool {
	switch model.Liveness(value) {
	case model.LivenessUnknown, model.LivenessAlive, model.LivenessBlocked, model.LivenessExited, model.LivenessUnreachable:
		return true
	default:
		return false
	}
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
	if opts.since != nil && execution.CreatedAt.Before(*opts.since) {
		return false
	}
	if opts.until != nil && !execution.CreatedAt.Before(*opts.until) {
		return false
	}
	if opts.caller != "" {
		caller := "unknown"
		if execution.Caller != nil {
			caller = string(execution.Caller.Harness)
		}
		if caller != opts.caller {
			return false
		}
	}
	if opts.adapter != "" && execution.Adapter != opts.adapter {
		return false
	}
	if opts.liveness != "" && string(execution.Liveness) != opts.liveness {
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
	item := recentExecution{Caller: execution.Caller, ID: execution.ID, Labels: labels, Authority: execution.Authority, Adapter: execution.Adapter, Mode: execution.Mode, State: execution.State, Liveness: execution.Liveness, CreatedAt: execution.CreatedAt, StartedAt: execution.StartedAt, UpdatedAt: execution.UpdatedAt, TerminalAt: execution.TerminalAt, DurationSeconds: duration.Seconds(), Unreconciled: acks.Unreconciled(execution)}
	if ack, ok := acks.ByID[execution.ID]; ok {
		acknowledgedAt := ack.AcknowledgedAt
		item.AcknowledgedAt = &acknowledgedAt
		item.AcknowledgementSource = ack.Source
	}
	return item
}
