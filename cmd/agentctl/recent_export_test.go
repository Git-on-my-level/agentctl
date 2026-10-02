package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

type recentExportDocument struct {
	Result struct {
		Executions []recentExecution  `json:"executions"`
		Count      int                `json:"count"`
		Total      int                `json:"total"`
		HasMore    bool               `json:"has_more"`
		Next       string             `json:"next_cursor"`
		AsOf       time.Time          `json:"as_of"`
		Summary    recentUsageSummary `json:"summary"`
	} `json:"result"`
}

func seedRecentExport(t *testing.T, path string, at time.Time, n int) {
	t.Helper()
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	for i := 0; i < n; i++ {
		execution := model.Execution{Authority: model.AuthorityNative, Mode: model.ModeDirect, Adapter: "codex", Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessUnreachable, CreatedAt: at, Capabilities: model.CapabilitySnapshot{NegotiatedAt: at, AdapterVersion: "0.1.0"}, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: at}, Labels: []string{"audit"}}
		if i%2 == 0 {
			execution.Caller = &model.ExecutionCaller{Harness: model.CallerHermes, Provenance: model.CallerDeclared}
		}
		if i == 1 {
			execution.Authority = model.AuthorityMultica
			execution.Mode = model.ModeMultica
			execution.Adapter = "multica"
		}
		if _, _, err := journal.CreateExecution(context.Background(), execution, contracts.MutationKey{}); err != nil {
			t.Fatal(err)
		}
	}
}

func readRecentExport(t *testing.T, path string, args ...string) (recentExportDocument, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	argv := append([]string{"--journal", path, "recent"}, args...)
	code := a.run(context.Background(), argv)
	var doc recentExportDocument
	if code == 0 {
		if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
	}
	return doc, stdout.String(), code
}

func TestRecentExportCursorWindowAndNoWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	seedRecentExport(t, path, at, 5)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	since, until := at.Format(time.RFC3339Nano), at.Add(time.Second).Format(time.RFC3339Nano)
	args := []string{"--limit", "2", "--since", since, "--until", until, "--label", "audit"}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 3; page++ {
		current := append([]string{}, args...)
		if cursor != "" {
			current = append(current, "--cursor", cursor)
		}
		doc, out, code := readRecentExport(t, path, current...)
		if code != 0 || doc.Result.Total != 5 {
			t.Fatalf("code=%d %s", code, out)
		}
		assertResultMatchesSchemaShape(t, out, "recent-result.schema.json")
		for _, x := range doc.Result.Executions {
			if seen[x.ID.String()] {
				t.Fatal("duplicate execution across pages")
			}
			seen[x.ID.String()] = true
		}
		if page < 2 && (!doc.Result.HasMore || doc.Result.Next == "") {
			t.Fatalf("missing next cursor: %s", out)
		}
		if page == 2 && (doc.Result.HasMore || doc.Result.Next != "") {
			t.Fatalf("unexpected next cursor: %s", out)
		}
		cursor = doc.Result.Next
	}
	if len(seen) != 5 {
		t.Fatalf("got %d records", len(seen))
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only export modified journal")
	}
	doc, out, code := readRecentExport(t, path, "--since", until, "--summary")
	if code != 0 || doc.Result.Total != 0 {
		t.Fatalf("exclusive window %d %s", code, out)
	}
	doc, out, code = readRecentExport(t, path, "--until", since, "--summary")
	if code != 0 || doc.Result.Total != 0 {
		t.Fatalf("exclusive until %d %s", code, out)
	}
}

func TestRecentExportCursorBindsScopeAndExcludesNewWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	seedRecentExport(t, path, time.Now().UTC().Add(-time.Hour), 3)
	first, out, code := readRecentExport(t, path, "--limit", "1")
	if code != 0 || first.Result.Next == "" {
		t.Fatalf("%d %s", code, out)
	}
	seedRecentExport(t, path, first.Result.AsOf.Add(time.Second), 1)
	second, out, code := readRecentExport(t, path, "--limit", "20", "--cursor", first.Result.Next)
	if code != 0 || second.Result.Total != 3 || second.Result.Count != 2 {
		t.Fatalf("cursor included new work: %d %s", code, out)
	}
	_, out, code = readRecentExport(t, path, "--cursor", first.Result.Next, "--state", "running")
	if code != 2 || !strings.Contains(out, "recent_invalid_cursor") {
		t.Fatalf("changed scope %d %s", code, out)
	}
	other := filepath.Join(t.TempDir(), "state", "journal.db")
	seedRecentExport(t, other, time.Now().UTC().Add(-time.Hour), 1)
	_, out, code = readRecentExport(t, other, "--cursor", first.Result.Next)
	if code != 2 || !strings.Contains(out, "different journal") {
		t.Fatalf("changed journal %d %s", code, out)
	}
}

func TestRecentExportSummaryCallerAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	seedRecentExport(t, path, time.Now().UTC().Add(-time.Hour), 5)
	doc, out, code := readRecentExport(t, path, "--summary", "--limit", "1")
	if code != 0 || doc.Result.Total != 5 || doc.Result.Count != 0 || doc.Result.HasMore || doc.Result.Summary.ByCaller["hermes"] != 3 || doc.Result.Summary.ByCaller["unknown"] != 2 || doc.Result.Summary.ByAuthority["multica"] != 1 {
		t.Fatalf("summary %d %s", code, out)
	}
	assertResultMatchesSchemaShape(t, out, "recent-result.schema.json", "summary")
	doc, out, code = readRecentExport(t, path, "--caller", "unknown")
	if code != 0 || doc.Result.Total != 2 {
		t.Fatalf("caller %d %s", code, out)
	}
	missing := filepath.Join(t.TempDir(), "state", "journal.db")
	for _, args := range [][]string{{"--since", "yesterday"}, {"--since", "2026-10-02T00:00:00Z", "--until", "2026-10-01T00:00:00Z"}, {"--cursor", "invalid"}, {"--cursor", strings.Repeat("x", 3000)}, {"--caller", "secret-value"}, {"--summary", "--cursor", "invalid"}} {
		_, out, code = readRecentExport(t, missing, args...)
		if code != 2 {
			t.Fatalf("args=%v code=%d %s", args, code, out)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatal("invalid read created journal")
		}
	}
}
