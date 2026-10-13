package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestStatusRecentProgressExportIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.db")
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
