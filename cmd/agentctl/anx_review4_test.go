package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/anxrun"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestReview4ANXStatusConfigurationPrivacy(t *testing.T) {
	secret := "glpat-abcdefghijklmnopqrst"
	for _, present := range []bool{false, true} {
		for _, format := range []string{"json", "text"} {
			root := testutil.TempDir(t)
			spoolPath := filepath.Join(root, secret, "private-spool")
			if present {
				if _, err := anxrun.OpenSpool(spoolPath); err != nil {
					t.Fatal(err)
				}
			}
			doc := map[string]any{"schema_version": 1, "anx": map[string]any{"run_links": map[string]any{"workspace_alias": secret, "spool_dir": spoolPath}}}
			raw, _ := json.Marshal(doc)
			configPath := filepath.Join(root, "config.json")
			if err := os.WriteFile(configPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			if code := a.run(context.Background(), []string{"--output", format, "--config", configPath, "--journal", filepath.Join(root, "journal.db"), "anx", "status"}); code != 0 {
				t.Fatalf("status exit %d: %s", code, stdout.String()+stderr.String())
			}
			out := stdout.String() + stderr.String()
			if strings.Contains(out, secret) || strings.Contains(out, root) {
				t.Fatal("configuration leaked in status")
			}
			if !strings.Contains(out, "[redacted]") {
				t.Fatal("unsafe alias was not redacted")
			}
			if format == "json" {
				var envelope struct {
					Result struct {
						Spool        string `json:"spool"`
						PendingCount int    `json:"pending_count"`
						DeadCount    int    `json:"dead_letter_count"`
					} `json:"result"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				want := "absent"
				if present {
					want = "present"
				}
				if envelope.Result.Spool != want || envelope.Result.PendingCount != 0 || envelope.Result.DeadCount != 0 {
					t.Fatal("status presence/counts incorrect")
				}
			} else if !strings.Contains(out, "pending_count=0") || !strings.Contains(out, "dead_letter_count=0") {
				t.Fatal("text status missing counts")
			}
		}
	}
}

func TestANXStatusDisclosesSafeConfiguration(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		root, err := os.MkdirTemp(os.TempDir(), "status-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		spoolPath := filepath.Join(root, "david", "private-spool")
		configPath := filepath.Join(root, "config.json")
		doc := map[string]any{"schema_version": 1, "anx": map[string]any{"run_links": map[string]any{"workspace_alias": "shared-work", "spool_dir": spoolPath}}}
		raw, _ := json.Marshal(doc)
		if err := os.WriteFile(configPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		if code := a.run(context.Background(), []string{"--output", format, "--config", configPath, "--journal", filepath.Join(root, "journal.db"), "anx", "status"}); code != 0 {
			t.Fatalf("status: %s%s", stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), spoolPath) || !strings.Contains(stdout.String(), "shared-work") {
			t.Fatalf("safe config hidden: %s", stdout.String())
		}
		if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
			t.Fatal("status created spool")
		}
	}
}
