package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// selfDefeatingErrorCode maps each state to the error class that an emitted
// next_action must never return: a recommendation that fails with the very
// condition that produced it leaves the caller in a loop with no progress.
func selfDefeatingErrorCode(state model.State) output.Code {
	switch state {
	case model.StateAttention:
		return output.CodeAttentionRequired
	case model.StateFailed:
		return output.CodeExecutionFailed
	case model.StateCancelled:
		return output.CodeExecutionCancelled
	case model.StateOrphaned:
		return output.CodeExecutionUnknown
	default:
		return ""
	}
}

func seedExecutionInState(t *testing.T, journalPath string, state model.State) model.Execution {
	t.Helper()
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	now := time.Now().UTC()
	liveness := model.LivenessAlive
	if state.Terminal() {
		liveness = model.LivenessExited
	}
	value := model.Execution{
		Authority: model.AuthorityNative, Adapter: "generic-process", Mode: model.ModeDirect,
		Acquisition: model.AcquisitionLaunched, State: state, Liveness: liveness,
		SourceBindings: []model.SourceBinding{},
		Capabilities:   model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		Observation:    model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now},
	}
	if state.Terminal() {
		terminal := now
		value.TerminalAt = &terminal
	}
	created, _, err := journal.CreateExecution(context.Background(), value, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

type nextActionDocument struct {
	OK          bool                 `json:"ok"`
	NextActions []output.NextAction  `json:"next_actions"`
	Warnings    []output.Warning     `json:"warnings"`
	Error       *nextActionErrorBody `json:"error"`
}

type nextActionErrorBody struct {
	Code        output.Code         `json:"code"`
	NextActions []output.NextAction `json:"next_actions"`
}

// runEmittedAction executes one emitted next_action in process against the same
// journal. Waiting actions are intentionally bounded: the assertion is about
// the returned error class, never about how long a legitimate wait blocks.
func runEmittedAction(t *testing.T, journalPath string, action output.NextAction) nextActionDocument {
	t.Helper()
	if len(action.Argv) == 0 || action.Argv[0] != "agentctl" {
		t.Fatalf("next_action argv is not an agentctl invocation: %v", action.Argv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	args := append([]string{"--journal", journalPath}, action.Argv[1:]...)
	a.run(ctx, args)
	var document nextActionDocument
	// --content writes raw text rather than a document; no such action is
	// emitted today, but decoding failures must not be silently ignored.
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("action %v produced undecodable output %q: %v", action.Argv, stdout.String(), err)
	}
	return document
}

func TestEmittedNextActionsNeverReturnTheirOwnStateError(t *testing.T) {
	for _, state := range []model.State{
		model.StateCreated, model.StateStarting, model.StateRunning, model.StateWaiting,
		model.StateAttention, model.StateCompleted, model.StateFailed, model.StateCancelled, model.StateOrphaned,
	} {
		t.Run(string(state), func(t *testing.T) {
			journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
			execution := seedExecutionInState(t, journalPath, state)
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "status", execution.ID.String()}); code != 0 {
				t.Fatalf("status exit=%d output=%s", code, stdout.String())
			}
			var status nextActionDocument
			if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if len(status.NextActions) == 0 {
				t.Fatalf("state %s emitted no next_action", state)
			}
			forbidden := selfDefeatingErrorCode(state)
			for _, action := range status.NextActions {
				document := runEmittedAction(t, journalPath, action)
				if document.OK || document.Error == nil {
					continue
				}
				if forbidden != "" && document.Error.Code == forbidden {
					t.Fatalf("state %s recommended %v, which returns %s again", state, action.Argv, document.Error.Code)
				}
			}
		})
	}
}

func TestAttentionOffersAnEscapeFromItsOwnStopCondition(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	execution := seedExecutionInState(t, journalPath, model.StateAttention)

	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "status", execution.ID.String()}); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, stdout.String())
	}
	var status nextActionDocument
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	assertAttentionActions(t, status.NextActions)
	authority := false
	for _, warning := range status.Warnings {
		if warning.Code == "attention_requires_authority_decision" {
			authority = true
		}
	}
	if !authority {
		t.Fatalf("status omitted the authority pointer: %s", stdout.String())
	}

	// The stop condition itself must carry the same escapes, because a caller
	// that reached attention through await never saw the status document.
	stdout.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "await", execution.ID.String(), "--timeout", "1s"}); code != 10 {
		t.Fatalf("await exit=%d output=%s", code, stdout.String())
	}
	var awaited nextActionDocument
	if err := json.Unmarshal(stdout.Bytes(), &awaited); err != nil {
		t.Fatal(err)
	}
	if awaited.Error == nil || awaited.Error.Code != output.CodeAttentionRequired {
		t.Fatalf("await document=%s", stdout.String())
	}
	assertAttentionActions(t, awaited.Error.NextActions)
}

func assertAttentionActions(t *testing.T, actions []output.NextAction) {
	t.Helper()
	waited, rechecked, evidence := false, false, false
	for _, action := range actions {
		argv := strings.Join(action.Argv, " ")
		switch {
		case strings.Contains(argv, "await") && strings.Contains(argv, "--ignore-attention"):
			waited = true
			if len(action.Preconditions) == 0 {
				t.Fatalf("continued wait omitted the authority precondition: %#v", action)
			}
		case strings.Contains(argv, "status"):
			rechecked = true
			if action.Mutates || action.SideEffectClass != output.ReadOnly {
				t.Fatalf("re-check is not read-only: %#v", action)
			}
		case strings.Contains(argv, "events") && strings.Contains(argv, "attention"):
			evidence = true
		}
		if strings.Contains(argv, "await") && !strings.Contains(argv, "--ignore-attention") {
			t.Fatalf("attention recommended a wait that stops on attention: %v", action.Argv)
		}
	}
	if !waited || !rechecked || !evidence {
		t.Fatalf("attention actions incomplete: %#v", actions)
	}
}

func TestInboxAttachesPerItemAttentionActions(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	seedExecutionInState(t, journalPath, model.StateAttention)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "inbox"}); code != 0 {
		t.Fatalf("inbox exit=%d output=%s", code, stdout.String())
	}
	var document struct {
		Result struct {
			Executions []inboxExecution `json:"executions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Result.Executions) != 1 {
		t.Fatalf("inbox=%s", stdout.String())
	}
	assertAttentionActions(t, document.Result.Executions[0].NextActions)
	for _, action := range document.Result.Executions[0].NextActions {
		if result := runEmittedAction(t, journalPath, action); result.Error != nil && result.Error.Code == output.CodeAttentionRequired {
			t.Fatalf("inbox recommended %v, which returns attention_required again", action.Argv)
		}
	}
}
