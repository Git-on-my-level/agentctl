package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

type sequenceGenerator struct{ next map[ids.Type]uint64 }

func (g *sequenceGenerator) New(typ ids.Type) (ids.ID, error) {
	value := g.next[typ]
	g.next[typ] = value + 1
	return ids.FromPayload(typ, value)
}

func openTestJournal(t *testing.T) (*Journal, *sequenceGenerator, time.Time) {
	t.Helper()
	generator := &sequenceGenerator{next: map[ids.Type]uint64{ids.TypeHost: 100, ids.TypeExecution: 200, ids.TypeEvent: 300}}
	now := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	journal, err := Open(path, Options{Generator: generator, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	return journal, generator, now
}

func TestOpenClassifiesLockTimeoutAsBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	first, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Open(path, Options{LockTimeout: 20 * time.Millisecond}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second open error=%v, want ErrBusy", err)
	}
}

func sampleExecution(now time.Time) model.Execution {
	return model.Execution{Authority: model.AuthorityNative, Adapter: "codex", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "0.1.0", Items: []model.CapabilityItem{}}, Supersedes: []ids.ExecutionID{}, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}}
}
func hash(ch byte) string { return "sha256:" + strings.Repeat(string(ch), 64) }

func semanticEvent(t *testing.T, adapterName string, projection any) (string, []byte) {
	t.Helper()
	key, canonical, err := callback.SemanticDedupeKey(adapterName, 1, projection)
	if err != nil {
		t.Fatal(err)
	}
	return key, canonical
}

func TestExecutionCRUDIdempotencyAndCAS(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	created, reused, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{Scope: "execution:create", Key: "parent/operation", InputDigest: hash('a')})
	if err != nil {
		t.Fatal(err)
	}
	if reused || created.Revision != 1 || created.ID.IsZero() || created.OriginHostID.IsZero() {
		t.Fatalf("created=%#v reused=%v", created, reused)
	}
	again, reused, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{Scope: "execution:create", Key: "parent/operation", InputDigest: hash('a')})
	if err != nil || !reused || again.ID != created.ID {
		t.Fatalf("again=%#v reused=%v err=%v", again, reused, err)
	}
	if _, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{Scope: "execution:create", Key: "parent/operation", InputDigest: hash('b')}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting idempotency err=%v", err)
	}
	created.State = model.StateWaiting
	updated, err := journal.UpdateExecution(ctx, created, 1)
	if err != nil || updated.Revision != 2 {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	if _, err := journal.UpdateExecution(ctx, updated, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update err=%v", err)
	}
	read, err := journal.GetExecution(ctx, created.ID)
	if err != nil || read.State != model.StateWaiting {
		t.Fatalf("read=%#v err=%v", read, err)
	}
}

func TestGetExecutionByMutationRecoversExactPreparedExecution(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	mutation := contracts.MutationKey{Scope: "dispatch:create", Key: "stable", InputDigest: hash('r')}
	created, _, err := journal.CreateExecution(ctx, sampleExecution(now), mutation)
	if err != nil {
		t.Fatal(err)
	}
	recovered, found, err := journal.GetExecutionByMutation(ctx, mutation)
	if err != nil || !found || recovered.ID != created.ID {
		t.Fatalf("recovered=%#v found=%v err=%v", recovered, found, err)
	}
	_, _, err = journal.GetExecutionByMutation(ctx, contracts.MutationKey{Scope: mutation.Scope, Key: mutation.Key, InputDigest: hash('x')})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("changed digest did not conflict: %v", err)
	}
}

func TestEventAppendIsAtomicContiguousAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	execution, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	key, projection := semanticEvent(t, execution.Adapter, map[string]any{"event": "started"})
	event := model.Event{ExecutionID: execution.ID, Authority: execution.Authority, Adapter: execution.Adapter, Ordering: model.OrderingObservation, Kind: model.EventStarted, ObservedAt: now, DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{"accepted": true}}
	first, reused, err := journal.AppendEvent(ctx, event, projection)
	if err != nil || reused || first.Sequence != 1 {
		t.Fatalf("first=%#v reused=%v err=%v", first, reused, err)
	}
	again, reused, err := journal.AppendEvent(ctx, event, projection)
	if err != nil || !reused || again.ID != first.ID || again.Sequence != 1 {
		t.Fatalf("again=%#v reused=%v err=%v", again, reused, err)
	}
	event.DedupeKey, projection = semanticEvent(t, execution.Adapter, map[string]any{"event": "progress"})
	event.Kind = model.EventProgress
	second, _, err := journal.AppendEvent(ctx, event, projection)
	if err != nil || second.Sequence != 2 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	events, err := journal.ListEvents(ctx, execution.ID, contracts.EventQuery{})
	if err != nil || len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	event.DedupeKey = key
	_, different := semanticEvent(t, execution.Adapter, map[string]any{"event": "different"})
	if _, _, err := journal.AppendEvent(ctx, event, different); !errors.Is(err, ErrConflict) {
		t.Fatalf("producer dedupe mismatch err=%v", err)
	}
}

func TestObservedEventStateAndCallbackCommitAtomically(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	execution, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	attention := model.StateAttention
	key, projection := semanticEvent(t, execution.Adapter, map[string]any{"attention": "permission"})
	event := model.Event{ExecutionID: execution.ID, Authority: execution.Authority, Adapter: execution.Adapter, Ordering: model.OrderingObservation, Kind: model.EventAttention, State: &attention, ObservedAt: now.Add(time.Second), DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{"attention_kind": "permission"}}
	next := execution
	next.State = model.StateAttention
	next.Liveness = model.LivenessBlocked
	next.UpdatedAt = event.ObservedAt
	next.Observation.ObservedAt = event.ObservedAt
	stored, storedEvent, reused, err := journal.CommitObservedEvent(ctx, next, execution.Revision, event, projection)
	if err != nil || reused || stored.State != model.StateAttention || stored.Liveness != model.LivenessBlocked || stored.Revision != execution.Revision+1 || storedEvent.Kind != model.EventAttention {
		t.Fatalf("stored=%#v event=%#v reused=%v err=%v", stored, storedEvent, reused, err)
	}
	again, duplicate, reused, err := journal.CommitObservedEvent(ctx, stored, stored.Revision, event, projection)
	if err != nil || !reused || again.Revision != stored.Revision || duplicate.ID != storedEvent.ID {
		t.Fatalf("again=%#v event=%#v reused=%v err=%v", again, duplicate, reused, err)
	}
	invalid := stored
	invalid.State = model.StateCompleted
	if _, _, _, err := journal.CommitObservedEvent(ctx, invalid, stored.Revision, event, projection); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal observed state err=%v", err)
	}
}

func TestOnlyOneTerminalEventIsAccepted(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	execution, _, _ := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	completed := model.StateCompleted
	key, projection := semanticEvent(t, execution.Adapter, map[string]any{"terminal": "completed"})
	event := model.Event{ExecutionID: execution.ID, Authority: execution.Authority, Adapter: execution.Adapter, Ordering: model.OrderingObservation, Kind: model.EventTerminal, State: &completed, ObservedAt: now, DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{"result_available": true}}
	if _, _, err := journal.AppendEvent(ctx, event, projection); err != nil {
		t.Fatal(err)
	}
	failed := model.StateFailed
	event.State = &failed
	event.DedupeKey, projection = semanticEvent(t, execution.Adapter, map[string]any{"terminal": "failed"})
	if _, _, err := journal.AppendEvent(ctx, event, projection); !errors.Is(err, ErrTerminalConflict) {
		t.Fatalf("terminal conflict err=%v", err)
	}
}

func TestTerminalOutcomeStateEventAndContentCommitAtomically(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	execution, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	terminalAt := now.Add(time.Second)
	next := execution
	next.State = model.StateCompleted
	next.Liveness = model.LivenessExited
	next.TerminalAt = &terminalAt
	next.UpdatedAt = terminalAt
	next.Observation.ObservedAt = terminalAt
	completed := model.StateCompleted
	key, projection := semanticEvent(t, execution.Adapter, map[string]any{"terminal": "outcome"})
	event := model.Event{ExecutionID: execution.ID, Authority: execution.Authority, Adapter: execution.Adapter, Ordering: model.OrderingObservation, Kind: model.EventTerminal, State: &completed, ObservedAt: terminalAt, DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{"availability": "stored", "outcome_execution_id": execution.ID.String()}}
	outcome := model.Outcome{SchemaVersion: 1, ExecutionID: execution.ID, Revision: 1, State: completed, Availability: model.OutcomeStored, RecordedAt: terminalAt, Source: execution.Adapter, ResultRef: "agentctl://" + execution.OriginHostID.String() + "/" + execution.ID.String(), Content: &model.OutcomeContent{MediaType: "text/plain", Text: "answer", Preview: "answer", Bytes: 6, SHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Truncated: false}}
	if _, _, _, _, err := journal.CommitTerminalOutcome(ctx, next, execution.Revision, outcome, event, projection); err == nil {
		t.Fatal("invalid outcome digest was accepted")
	}
	unchanged, err := journal.GetExecution(ctx, execution.ID)
	if err != nil || unchanged.State != model.StateRunning {
		t.Fatalf("failed commit changed execution: %#v err=%v", unchanged, err)
	}
	if events, err := journal.ListEvents(ctx, execution.ID, contracts.EventQuery{}); err != nil || len(events) != 0 {
		t.Fatalf("failed commit left events: %#v err=%v", events, err)
	}
	sum := sha256.Sum256([]byte("answer"))
	outcome.Content.SHA256 = "sha256:" + hex.EncodeToString(sum[:])
	storedExecution, storedOutcome, storedEvent, reused, err := journal.CommitTerminalOutcome(ctx, next, execution.Revision, outcome, event, projection)
	if err != nil || reused || storedExecution.State != model.StateCompleted || storedOutcome.Content == nil || storedEvent.Kind != model.EventTerminal {
		t.Fatalf("commit execution=%#v outcome=%#v event=%#v reused=%v err=%v", storedExecution, storedOutcome, storedEvent, reused, err)
	}
	readOutcome, err := journal.GetOutcome(ctx, execution.ID)
	if err != nil || readOutcome.Content == nil || readOutcome.Content.Text != "answer" {
		t.Fatalf("read outcome=%#v err=%v", readOutcome, err)
	}
	progressKey, progressProjection := semanticEvent(t, execution.Adapter, map[string]any{"progress": "late"})
	progress := model.Event{ExecutionID: execution.ID, Authority: execution.Authority, Adapter: execution.Adapter, Ordering: model.OrderingObservation, Kind: model.EventProgress, ObservedAt: terminalAt.Add(time.Second), DedupeKey: progressKey, DedupeVersion: 1}
	if _, _, err := journal.AppendEvent(ctx, progress, progressProjection); !errors.Is(err, ErrTerminalConflict) {
		t.Fatalf("late nonterminal event err=%v", err)
	}
}

func TestOwnerOnlyModesAndReadOnlyOpenDoNotCreate(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing", "journal.db")
	if _, err := Open(missing, Options{ReadOnly: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read-only missing err=%v", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only open created root: %v", err)
	}
	path := filepath.Join(root, "private", "journal.db")
	journal, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	dirInfo, _ := os.Stat(filepath.Dir(path))
	fileInfo, _ := os.Stat(path)
	if dirInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes dir=%04o file=%04o", dirInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{ReadOnly: true}); !errors.Is(err, ErrPermission) {
		t.Fatalf("unsafe file err=%v", err)
	}
}

func TestTerminalCannotReturnToRunning(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	execution, _, _ := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	terminal := now.Add(time.Second)
	execution.State = model.StateCompleted
	execution.Liveness = model.LivenessExited
	execution.TerminalAt = &terminal
	execution.UpdatedAt = terminal
	execution.Observation.ObservedAt = terminal
	execution, err := journal.UpdateExecution(ctx, execution, 1)
	if err != nil {
		t.Fatal(err)
	}
	execution.State = model.StateRunning
	execution.TerminalAt = nil
	if _, err := journal.UpdateExecution(ctx, execution, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal regression err=%v", err)
	}
}
