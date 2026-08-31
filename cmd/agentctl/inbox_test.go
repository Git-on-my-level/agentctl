package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestInboxCommandExplainsWithoutReadingResultContent(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	native := `{"type":"result","status":"completed","result":"INBOX_PRIVATE_RESULT"}`
	if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", native}); code != 0 {
		t.Fatalf("run exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "inbox"}); code != 0 {
		t.Fatalf("inbox exit=%d output=%s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), "INBOX_PRIVATE_RESULT") {
		t.Fatalf("inbox leaked result content: %s", stdout.String())
	}
	var document struct {
		Result struct {
			Executions []inboxExecution `json:"executions"`
			Count      int              `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Result.Count != 1 || len(document.Result.Executions) != 1 {
		t.Fatalf("inbox=%s", stdout.String())
	}
	if got := inboxReasonCodes(document.Result.Executions[0].Reasons); !equalStrings(got, []string{"result_unreconciled"}) {
		t.Fatalf("reasons=%v", got)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "text", "--journal", journalPath, "inbox"}); code != 0 {
		t.Fatalf("text inbox exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `why=["result_unreconciled"]`) || strings.Contains(stdout.String(), "INBOX_PRIVATE_RESULT") {
		t.Fatalf("text inbox=%s", stdout.String())
	}
}

func TestInboxSeparatesWorkFromToolHealth(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	execution := model.Execution{
		State:       model.StateRunning,
		Liveness:    model.LivenessUnreachable,
		CreatedAt:   now.Add(-3 * time.Hour),
		UpdatedAt:   now.Add(-2 * time.Hour),
		Observation: model.Observation{ObservedAt: now.Add(-2 * time.Hour)},
	}
	item, actionable := projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{})
	if !actionable {
		t.Fatal("stale unreachable work was not actionable")
	}
	if item.WorkHealth != "observation_stale" || item.ToolHealth != "unreachable" {
		t.Fatalf("health projection=%#v", item)
	}
	want := []string{"running_observation_stale", "tool_unreachable"}
	if got := inboxReasonCodes(item.Reasons); !equalStrings(got, want) {
		t.Fatalf("reasons=%v want=%v", got, want)
	}
	if item.Reasons[1].Summary != "the runtime is unreachable; this does not prove the work failed" {
		t.Fatalf("unreachable reason overclaims task failure: %#v", item.Reasons[1])
	}
}

func TestInboxTerminalFailureClearsOnCollection(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	terminalAt := now.Add(-time.Minute)
	execution := model.Execution{
		State:       model.StateFailed,
		Liveness:    model.LivenessExited,
		CreatedAt:   now.Add(-time.Hour),
		UpdatedAt:   terminalAt,
		TerminalAt:  &terminalAt,
		Observation: model.Observation{ObservedAt: terminalAt},
	}
	acks := store.AcknowledgementIndex{Epoch: now.Add(-2 * time.Hour), ByID: map[ids.ExecutionID]store.ExecutionAcknowledgement{}}
	item, actionable := projectInbox(execution, now, time.Hour, acks)
	if !actionable || item.WorkHealth != "failed" || !item.Unreconciled {
		t.Fatalf("uncollected failure=%#v actionable=%v", item, actionable)
	}
	want := []string{"execution_failed", "result_unreconciled"}
	if got := inboxReasonCodes(item.Reasons); !equalStrings(got, want) {
		t.Fatalf("reasons=%v want=%v", got, want)
	}
	acks.ByID[execution.ID] = store.ExecutionAcknowledgement{ExecutionID: execution.ID, AcknowledgedAt: now, Source: store.AcknowledgementResult}
	if _, actionable := projectInbox(execution, now, time.Hour, acks); actionable {
		t.Fatal("acknowledged terminal failure remained in the inbox")
	}
}

func TestInboxAttentionDoesNotRequireStaleness(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	execution := model.Execution{State: model.StateAttention, Liveness: model.LivenessBlocked, CreatedAt: now, UpdatedAt: now, Observation: model.Observation{ObservedAt: now}}
	item, actionable := projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{})
	if !actionable || item.WorkHealth != "attention_required" || item.ToolHealth != "blocked" {
		t.Fatalf("attention=%#v actionable=%v", item, actionable)
	}
	if got := inboxReasonCodes(item.Reasons); !equalStrings(got, []string{"attention_required"}) {
		t.Fatalf("reasons=%v", got)
	}
}

func TestInboxUnreachableWaitingDoesNotInventWorkFailure(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	execution := model.Execution{State: model.StateWaiting, Liveness: model.LivenessUnreachable, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour), Observation: model.Observation{ObservedAt: now.Add(-2 * time.Hour)}}
	item, actionable := projectInbox(execution, now, time.Hour, store.AcknowledgementIndex{})
	if !actionable || item.WorkHealth != "active" || item.ToolHealth != "unreachable" {
		t.Fatalf("waiting unreachable=%#v actionable=%v", item, actionable)
	}
	if got := inboxReasonCodes(item.Reasons); !equalStrings(got, []string{"tool_unreachable"}) {
		t.Fatalf("reasons=%v", got)
	}
}

func TestParseInboxBoundsStalenessAndLimit(t *testing.T) {
	opts, problem := parseInbox([]string{"--stale-after", "2h", "--limit", "7", "--adapter", "codex", "--label", "review"})
	if problem != nil {
		t.Fatal(problem)
	}
	if opts.staleAfter != 2*time.Hour || opts.limit != 7 || opts.adapter != "codex" || !equalStrings(opts.labels, []string{"review"}) {
		t.Fatalf("options=%#v", opts)
	}
	for _, value := range []string{"59s", "721h", "forever"} {
		if _, problem := parseInbox([]string{"--stale-after", value}); problem == nil {
			t.Fatalf("--stale-after %q accepted", value)
		}
	}
}

func inboxReasonCodes(reasons []inboxReason) []string {
	codes := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		codes = append(codes, reason.Code)
	}
	return codes
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
