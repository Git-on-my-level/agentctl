package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestSubscribeCreateNormalizesDirectAuthorityAndRejectsUnknown(t *testing.T) {
	root, err := os.MkdirTemp(".", ".subscription-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "journal.db")
	destination := filepath.Join(root, "events.ndjson")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"--journal", journal, "subscribe", "create", "--authority", "direct", "--destination", "file", "--target", destination})
	if code != 0 {
		t.Fatalf("create exit=%d output=%s", code, stdout.String())
	}
	var document struct {
		Result struct {
			Subscription struct {
				Filter struct {
					Authority string `json:"authority"`
				} `json:"filter"`
			} `json:"subscription"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Result.Subscription.Filter.Authority != "native" {
		t.Fatalf("authority=%q output=%s", document.Result.Subscription.Filter.Authority, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journal, "subscribe", "create", "--authority", "typo", "--destination", "file", "--target", destination}); code == 0 {
		t.Fatalf("unknown authority accepted: %s", stdout.String())
	}
}

func TestSubscribeLabelPrefixDeliversFutureLabeledExecutions(t *testing.T) {
	root := testutil.TempDir(t)
	journalPath := filepath.Join(root, "state", "journal.db")
	// Delivery needs an owner-only directory; create makes a missing one 0700.
	destination := filepath.Join(root, "events", "events.ndjson")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	for _, test := range []struct {
		args []string
		code output.Code
	}{
		{args: []string{"--label-prefix", "lane.", "--execution", "exec-purple-monkey-dragon-river-candle-meadow"}, code: output.CodeUsage},
		{args: []string{"--label-prefix", "Lane/"}, code: output.CodeUsage},
		{args: []string{"--label-prefix", "lane.", "--kind", "terminal,stall"}, code: output.CodeCapabilityUnavailable},
	} {
		stdout.Reset()
		args := append([]string{"--journal", journalPath, "subscribe", "create", "--destination", "file", "--target", destination}, test.args...)
		if code := a.run(context.Background(), args); code != output.ExitCodeFor(test.code) {
			t.Fatalf("args=%v exit=%d output=%s", test.args, code, stdout.String())
		}
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "subscribe", "create", "--label-prefix", "lane.", "--kind", "terminal", "--destination", "file", "--target", destination}); code != 0 {
		t.Fatalf("create exit=%d output=%s", code, stdout.String())
	}
	var created struct {
		Result struct {
			Subscription subscription.Subscription `json:"subscription"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	value := created.Result.Subscription
	if value.Filter.LabelPrefix != "lane." || len(value.Filter.ExecutionIDs) != 0 || value.AutoExpireOnTerminal {
		t.Fatalf("label-prefix subscription=%s", stdout.String())
	}
	matching := completedRun(t, journalPath, "lane.one")
	completedRun(t, journalPath, "elsewhere")
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	pending, err := journal.ListPendingDeliveries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SubscriptionID != value.ID || pending[0].ExecutionID != matching {
		t.Fatalf("pending deliveries=%#v", pending)
	}
}

// A file destination in a group- or world-readable directory can never be
// delivered (delivery requires an owner-only directory), so create refuses it
// up front instead of retrying silently until the subscription expires.
func TestSubscribeCreateRejectsNonOwnerOnlyFileDirectory(t *testing.T) {
	root := testutil.TempDir(t)
	journalPath := filepath.Join(root, "state", "journal.db")
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "subscribe", "create", "--label-prefix", "lane.", "--destination", "file", "--target", filepath.Join(shared, "events.ndjson")})
	if code != output.ExitCodeFor(output.CodeUsage) || !strings.Contains(stdout.String(), "owner-only") || !strings.Contains(stdout.String(), "chmod 700 "+shared) {
		t.Fatalf("non-owner-only directory exit=%d output=%s", code, stdout.String())
	}
	fresh := filepath.Join(root, "fresh", "nested")
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "subscribe", "create", "--label-prefix", "lane.", "--destination", "file", "--target", filepath.Join(fresh, "events.ndjson")}); code != 0 {
		t.Fatalf("missing directory exit=%d output=%s", code, stdout.String())
	}
	info, err := os.Stat(fresh)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("created destination directory mode=%v err=%v", info, err)
	}
}
