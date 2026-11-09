package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestJournalRetentionPolicyAndHostLocalPreservation(t *testing.T) {
	var absent *Journal
	if enabled, age := absent.RetentionPolicy(); !enabled || age != 14*24*time.Hour {
		t.Fatalf("defaults=%v %s", enabled, age)
	}
	off := false
	policy := &Journal{Retention: &JournalRetention{Enabled: &off, Days: 30}}
	cfg := testBootstrapConfig()
	cfg.Journal = policy
	path := filepath.Join(testutil.TempDir(t), "config.json")
	if err := Save(path, cfg, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if enabled, age := loaded.Journal.RetentionPolicy(); enabled || age != 30*24*time.Hour {
		t.Fatalf("round trip=%v %s", enabled, age)
	}
	// Source sync must preserve the local kill switch even without bootstrap.
	materialized := applyHostLocalConfig(loaded, testBootstrapConfig())
	if enabled, _ := materialized.Journal.RetentionPolicy(); enabled {
		t.Fatal("source sync cleared retention kill switch")
	}
	for _, days := range []int{-1, 3651} {
		if err := (Journal{Retention: &JournalRetention{Days: days}}).Validate(); err == nil {
			t.Fatalf("accepted days=%d", days)
		}
	}
}
