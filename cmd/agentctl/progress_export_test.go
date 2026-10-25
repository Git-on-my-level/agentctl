package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestStatusRecentProgressExportIsReadOnly(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	seedRecentExport(t, path, at, 2)
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := journal.ListExecutions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	// Only new evidence is populated. An older row remains explicitly unknown.
	progress := at.Add(time.Minute)
	rows[0].ProgressAt = &progress
	if _, err := journal.UpdateExecution(context.Background(), rows[0], rows[0].Revision); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, out, code := readRecentExport(t, path)
	if code != 0 {
		t.Fatalf("recent exit=%d: %s", code, out)
	}
	assertResultMatchesSchemaShape(t, out, "recent-result.schema.json")
	var doc struct {
		Result struct {
			Executions []map[string]json.RawMessage `json:"executions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	known, unknown := 0, 0
	for _, row := range doc.Result.Executions {
		value, ok := row["progress_at"]
		if !ok {
			t.Fatal("recent omitted progress_at")
		}
		if string(value) == "null" {
			unknown++
			continue
		}
		var got time.Time
		if err := json.Unmarshal(value, &got); err != nil || !got.Equal(progress) {
			t.Fatalf("progress=%s err=%v", value, err)
		}
		known++
	}
	if known != 1 || unknown != 1 {
		t.Fatalf("known=%d unknown=%d", known, unknown)
	}
	for _, row := range rows {
		var stdout, stderr bytes.Buffer
		app := testApp(&stdout, &stderr)
		if code := app.run(context.Background(), []string{"--journal", path, "status", row.ID.String()}); code != 0 {
			t.Fatalf("status exit=%d: %s", code, stdout.String())
		}
		var status struct {
			Result map[string]json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		value, ok := status.Result["progress_at"]
		if !ok {
			t.Fatal("status omitted progress_at")
		}
		if row.ProgressAt == nil && string(value) != "null" {
			t.Fatalf("status invented legacy progress: %s", value)
		}
		if row.ProgressAt != nil {
			var got time.Time
			if err := json.Unmarshal(value, &got); err != nil || !got.Equal(progress) {
				t.Fatalf("status progress=%s err=%v", value, err)
			}
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("status/recent progress export mutated journal")
	}
}

func TestNativeProgressWritersExcludeHeartbeats(t *testing.T) {
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	journal, err := store.Open(filepath.Join(testutil.TempDir(t), "state", "journal.db"), store.Options{Clock: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	execution, _, err := journal.CreateExecution(ctx, model.Execution{Authority: model.AuthorityNative, Mode: model.ModeDirect, Adapter: "generic-process", Acquisition: model.AcquisitionLaunched, State: model.StateStarting, Liveness: model.LivenessUnknown, Capabilities: model.CapabilitySnapshot{NegotiatedAt: at, AdapterVersion: "test"}, Observation: model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityUnknown, ObservedAt: at}}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	started := at.Add(time.Second)
	execution, err = applySession(journal, execution, adapter.Session{State: adapter.StateRunning, Liveness: adapter.LivenessAlive, Observation: adapter.Observation{ObservedAt: started}}, started)
	if err != nil {
		t.Fatal(err)
	}
	assertProgress := func(want time.Time) {
		t.Helper()
		if execution.ProgressAt == nil || !execution.ProgressAt.Equal(want) {
			t.Fatalf("progress=%v want=%s", execution.ProgressAt, want)
		}
	}
	assertProgress(started)
	progressed := at.Add(2 * time.Second)
	execution, err = appendNativeEvent(ctx, journal, execution, adapter.Event{Kind: "progress", State: adapter.StateRunning, SourceState: "tool.started", ObservedAt: progressed})
	if err != nil {
		t.Fatal(err)
	}
	assertProgress(progressed)
	execution, err = refreshRunnerLease(journal, execution, at.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertProgress(progressed)
	execution, err = appendNativeEvent(ctx, journal, execution, adapter.Event{Kind: "progress", State: adapter.StateRunning, SourceState: "tool.started", Cursor: "second", ObservedAt: at.Add(4 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	assertProgress(progressed)
	finished := at.Add(5 * time.Second)
	execution, err = a.finalizeResultConverging(ctx, common{}, journal, execution, adapter.Result{State: adapter.StateCompleted, Success: true, Content: "answer", ContentType: "text/plain"}, finished, false)
	if err != nil {
		t.Fatal(err)
	}
	assertProgress(finished)
}

// Activity-source observations repeat one source state but each is new agent
// activity, so every one advances progress_at (a silent-stdout run otherwise
// looks frozen after its first activity event).
func TestNativeActivityObservationsKeepAdvancingProgress(t *testing.T) {
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	journal, err := store.Open(filepath.Join(testutil.TempDir(t), "state", "journal.db"), store.Options{Clock: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	execution, _, err := journal.CreateExecution(ctx, model.Execution{Authority: model.AuthorityNative, Mode: model.ModeDirect, Adapter: "generic-process", Acquisition: model.AcquisitionLaunched, State: model.StateStarting, Liveness: model.LivenessUnknown, Capabilities: model.CapabilitySnapshot{NegotiatedAt: at, AdapterVersion: "test"}, Observation: model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityUnknown, ObservedAt: at}}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	execution, err = applySession(journal, execution, adapter.Session{State: adapter.StateRunning, Liveness: adapter.LivenessAlive, Observation: adapter.Observation{ObservedAt: at}}, at)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		observed := at.Add(time.Duration(i) * 15 * time.Second)
		execution, err = appendNativeEvent(ctx, journal, execution, adapter.Event{Kind: "progress", State: adapter.StateRunning, SourceState: "zcode.activity.observed", Cursor: fmt.Sprint(i), ObservedAt: observed, Payload: map[string]any{"family": "zcode", "activity_events": i}})
		if err != nil {
			t.Fatal(err)
		}
		if execution.ProgressAt == nil || !execution.ProgressAt.Equal(observed) {
			t.Fatalf("activity %d: progress=%v want=%s", i, execution.ProgressAt, observed)
		}
	}
}
