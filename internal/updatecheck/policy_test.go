package updatecheck

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultPathsAreBinaryGlobal(t *testing.T) {
	root := t.TempDir()
	getenv := func(name string) string {
		if name == "XDG_STATE_HOME" {
			return root
		}
		return ""
	}
	state, policy, err := DefaultPaths(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if state != filepath.Join(root, "agentctl", "update-state.json") || policy != filepath.Join(root, "agentctl", "update-policy.json") {
		t.Fatalf("state=%s policy=%s", state, policy)
	}
	if _, err := os.Stat(filepath.Join(root, "agentctl")); !os.IsNotExist(err) {
		t.Fatalf("path discovery mutated state: %v", err)
	}
}

func TestPolicyDefaultsAutoAndPersistsExplicitModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "update-policy.json")
	if mode, err := ResolveMode(path, func(string) string { return "" }); err != nil || mode != ModeAuto {
		t.Fatalf("default mode=%q err=%v", mode, err)
	}
	for _, want := range []Mode{ModeNotify, ModeOff, ModeAuto} {
		if err := SetMode(path, want); err != nil {
			t.Fatal(err)
		}
		if got, err := ResolveMode(path, func(string) string { return "" }); err != nil || got != want {
			t.Fatalf("mode=%q want=%q err=%v", got, want, err)
		}
	}
}

func TestDueReadsStateWithoutMutation(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state", "update-state.json")
	options := Options{CurrentVersion: "v0.3.3", StatePath: path, Now: func() time.Time { return now }}
	if !Due(options) {
		t.Fatal("missing state should be due")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("due check mutated state: %v", err)
	}
	if err := writeState(path, cacheState{SchemaVersion: stateSchema, CheckedOn: "2026-08-18"}); err != nil {
		t.Fatal(err)
	}
	if Due(options) {
		t.Fatal("completed daily check remained due")
	}
}
