package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestRecentExportCursorSurvivesBoundaryRowDeletion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state", "journal.db")
	seedRecentExport(t, path, time.Now().UTC().Add(-time.Hour), 5)
	whole, out, code := readRecentExport(t, path, "--limit", "20")
	if code != 0 || whole.Result.Count != 5 {
		t.Fatalf("seed export exit=%d %s", code, out)
	}
	first, out, code := readRecentExport(t, path, "--limit", "2")
	if code != 0 || first.Result.Count != 2 || first.Result.Next == "" {
		t.Fatalf("first page exit=%d %s", code, out)
	}
	boundary := first.Result.Executions[1].ID.String()
	// Direct bucket deletion is confined to this disposable fixture. It models
	// retention removing the boundary row without depending on result-collection
	// policy or altering timestamps of the rows that should remain pageable.
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("executions")).Delete([]byte(boundary))
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := recentFixtureSnapshot(t, root)
	seen := map[string]bool{first.Result.Executions[0].ID.String(): true, boundary: true}
	cursor := first.Result.Next
	remaining := 0
	for page := 0; page < 2; page++ {
		doc, out, code := readRecentExport(t, path, "--limit", "2", "--cursor", cursor)
		if code != 0 || doc.Result.Total != 4 {
			t.Fatalf("cursor stranded or live total incorrect: exit=%d %s", code, out)
		}
		for _, item := range doc.Result.Executions {
			if seen[item.ID.String()] {
				t.Fatal("cursor replayed a previous page or deleted boundary")
			}
			seen[item.ID.String()] = true
			remaining++
		}
		if doc.Result.HasMore != (page == 0) || (doc.Result.Next != "") != doc.Result.HasMore {
			t.Fatalf("unexpected continuation after boundary deletion: %s", out)
		}
		cursor = doc.Result.Next
	}
	if remaining != 3 || len(seen) != 5 {
		t.Fatalf("missed surviving rows: remaining=%d seen=%d", remaining, len(seen))
	}
	for _, item := range whole.Result.Executions {
		if !seen[item.ID.String()] {
			t.Fatalf("missed %s after deletion", item.ID)
		}
	}
	if after := recentFixtureSnapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Fatal("paging after boundary deletion modified the journal or fixture directory")
	}
}
