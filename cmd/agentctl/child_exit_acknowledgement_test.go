package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"go.etcd.io/bbolt"
)

// This exercises the snapshot selector through the real CLI, rather than only
// the authoritative per-parent query it used to bypass on forged collection.
func TestChildExitInboxValidatesCollectionSnapshot(t *testing.T) {
	for _, variant := range []string{"wrong-key", "invalid-key", "wrong-record-id", "invalid-source", "missing-source", "missing-time", "invalid-time", "null", "nested-bucket", "uncollected", "result", "await", "bulk_reconciled", "running-with-stamp"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			f := newDelegateFixture(t, continueAgent)
			j, err := store.Open(f.journal, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			parent := identityFixture(t, j, "codex", "inbox-collection-snapshot")
			child := addExitChild(t, j, parent, variant != "running-with-stamp")
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			c := common{configPath: f.config, journalPath: f.journal}
			parent, err = a.finalizeResult(ctx, c, j, parent, adapter.Result{Success: true, State: adapter.StateCompleted, Content: "parent answer"}, time.Now().UTC(), false)
			if err != nil {
				t.Fatal(err)
			}
			if !parent.ChildExitRecheck || len(parent.UncollectedChildren) != 1 {
				t.Fatal("fixture lacks immutable exit evidence")
			}
			if _, _, err = j.AcknowledgeExecution(ctx, parent.ID, store.AcknowledgementAwait); err != nil {
				t.Fatal(err)
			}
			if err = j.Close(); err != nil {
				t.Fatal(err)
			}
			// Raw fixture mutation deliberately models corrupt/legacy evidence. No
			// production writer may bypass Journal.update certification.
			db, err := bbolt.Open(f.journal, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bbolt.Tx) error {
				if variant == "uncollected" {
					return nil
				}
				key := []byte(child.ID.String())
				wrong, _ := ids.FromPayload(ids.TypeExecution, 123456789)
				ack := store.ExecutionAcknowledgement{ExecutionID: child.ID, AcknowledgedAt: time.Now().UTC(), Source: store.AcknowledgementAwait}
				switch variant {
				case "wrong-key":
					key = []byte(wrong.String())
				case "invalid-key":
					key = []byte("not-an-execution")
				case "wrong-record-id":
					ack.ExecutionID, _ = ids.ParseExecutionID(wrong.String())
				case "invalid-source":
					ack.Source = "unverified"
				case "missing-source":
					ack.Source = ""
				case "missing-time":
					ack.AcknowledgedAt = time.Time{}
				case "result", "await", "bulk_reconciled":
					ack.Source = variant
				}
				b := tx.Bucket([]byte("execution_acknowledgements"))
				if variant == "nested-bucket" {
					_, err := b.CreateBucket(key)
					return err
				}
				raw, err := json.Marshal(ack)
				if err != nil {
					return err
				}
				if variant == "null" {
					raw = []byte("null")
				}
				if variant == "invalid-time" {
					raw = bytes.Replace(raw, []byte(ack.AcknowledgedAt.Format(time.RFC3339Nano)), []byte("invalid-time"), 1)
				}
				return b.Put(key, raw)
			})
			closeErr := db.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("fixture: %v %v", err, closeErr)
			}
			before, err := os.ReadFile(f.journal)
			if err != nil {
				t.Fatal(err)
			}
			stdout.Reset()
			stderr.Reset()
			a.getenv = func(string) string { return "" }
			a.updateNotice = func(context.Context, string, common) *output.Warning { t.Fatal("read invoked maintenance"); return nil }
			code := a.run(ctx, []string{"--journal", f.journal, "--config", f.config, "inbox", "--json", "--limit", "200"})
			valid := variant == "uncollected" || variant == "result" || variant == "await" || variant == "bulk_reconciled" || variant == "running-with-stamp"
			if !valid {
				var doc output.ErrorDocument
				if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
					t.Fatalf("non-JSON failure: %v %s", err, stdout.String())
				}
				if code != 70 || doc.OK || doc.Error == nil || doc.Error.Code != output.CodeInternal || doc.Error.ExitCode != 70 || doc.Error.Retryable || doc.Error.Message != "list execution acknowledgements" || doc.Error.Details["diagnostic_code"] != "inbox_acknowledgement_corrupt" || doc.Error.Details["complete"] != false || bytes.Contains(stdout.Bytes(), []byte(`"result":`)) {
					t.Fatalf("wrong structured failure: exit=%d %s", code, stdout.String())
				}
			} else {
				var doc struct {
					OK     bool `json:"ok"`
					Result struct {
						Executions []inboxExecution `json:"executions"`
						Total      int              `json:"total"`
					} `json:"result"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
					t.Fatal(err)
				}
				if code != 0 || !doc.OK {
					t.Fatalf("valid snapshot failed: exit=%d %s", code, stdout.String())
				}
				found := false
				for _, item := range doc.Result.Executions {
					if item.ID == parent.ID {
						found = true
						if len(item.UncollectedChildren) != 1 || item.UncollectedChildren[0].ID != child.ID {
							t.Fatalf("lost child: %+v", item)
						}
					}
				}
				active := variant == "uncollected" || variant == "running-with-stamp"
				if found != active || (!active && doc.Result.Total != 0) {
					t.Fatalf("collection control active=%t: %s", active, stdout.String())
				}
			}
			after, err := os.ReadFile(f.journal)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("read-only inbox altered journal: %v", err)
			}
		})
	}
}
