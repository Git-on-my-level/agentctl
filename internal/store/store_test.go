package store

import (
	"context"
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
