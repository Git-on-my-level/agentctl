package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
