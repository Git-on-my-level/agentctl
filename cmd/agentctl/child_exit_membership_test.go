package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"go.etcd.io/bbolt"
)

func TestChildExitInboxRequiresCompleteRetainedMembership(t *testing.T) {
	for _, variant := range []string{
		"retained-uncollected", "retained-collected", "missing", "missing-collected", "missing-covered-exit",
		"expired-compact", "expired-full", "expired-wrong-id", "expired-wrong-scope", "expired-wrong-key", "expired-retained",
		"expired-scope-only", "expired-key-only", "expired-scope-null", "expired-key-null", "expired-both-null",
		"expired-scope-empty", "expired-key-empty", "expired-both-empty",
		"expired-null-scope-only", "expired-null-key-only", "expired-empty-scope-only", "expired-empty-key-only",
		"expired-missing-digest", "expired-empty-digest", "expired-null-digest", "expired-invalid-digest",
		"expired-compact-missing-digest", "expired-compact-empty-digest", "expired-compact-null-digest", "expired-compact-invalid-digest",
		"expired-short-digest", "expired-uppercase-digest", "expired-unprefixed-digest", "expired-nonstring-digest",
		"expired-duplicate-scope", "expired-duplicate-key", "expired-duplicate-digest", "expired-duplicate-type", "expired-duplicate-id",
		"expired-duplicate-scope-case", "expired-duplicate-scope-escape", "expired-duplicate-scope-unicode",
		"expired-compact-duplicate-digest", "expired-duplicate-extension", "expired-duplicate-unknown",
		"expired-compact-forward", "expired-full-forward",
	} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			f := newDelegateFixture(t, continueAgent)
			j, err := store.Open(f.journal, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			parent := identityFixture(t, j, "codex", "membership-inbox")
			child := addExitChild(t, j, parent, variant != "missing-covered-exit")
			var waiter *store.Waiter
			if variant == "missing-covered-exit" {
				waiter, err = store.RegisterWaiter(ctx, f.journal, child.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer waiter.Close()
			}
			var out, stderr bytes.Buffer
			a := testApp(&out, &stderr)
			c := common{configPath: f.config, journalPath: f.journal}
			parent, err = a.finalizeResult(ctx, c, j, parent, adapter.Result{Success: true, State: adapter.StateCompleted, Content: "parent answer"}, time.Now().UTC(), false)
			if err != nil {
				t.Fatal(err)
			}
			if !parent.ChildExitRecheck {
				t.Fatal("fixture lacks durable recheck marker")
			}
			if variant == "missing-covered-exit" {
				if parent.ChildExitGuard != nil || len(parent.UncollectedChildren) != 0 {
					t.Fatal("covered exit invented outstanding work")
				}
				if err := waiter.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err = j.AcknowledgeExecution(ctx, parent.ID, store.AcknowledgementAwait); err != nil {
				t.Fatal(err)
			}
			if variant == "retained-collected" || variant == "missing-collected" {
				if _, _, err = j.AcknowledgeExecution(ctx, child.ID, store.AcknowledgementAwait); err != nil {
					t.Fatal(err)
				}
			}
			if err = j.Close(); err != nil {
				t.Fatal(err)
			}
			// Raw mutation is intentional corrupt/legacy fixture evidence only.
			db, err := bbolt.Open(f.journal, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bbolt.Tx) error {
				if strings.HasPrefix(variant, "retained-") {
					return nil
				}
				if variant != "expired-retained" {
					if err := tx.Bucket([]byte("executions")).Delete([]byte(child.ID.String())); err != nil {
						return err
					}
				}
				if strings.HasPrefix(variant, "expired-") {
					scope := "execution:children:parent:" + parent.ID.String()
					// Preserve the actual attribution producer's digest/metadata, rather
					// than inventing a valid expiry record from zero-valued fields.
					indexKey := []byte(scope + "\x00" + child.ID.String())
					var record map[string]any
					if err := json.Unmarshal(tx.Bucket([]byte("idempotency")).Get(indexKey), &record); err != nil {
						return err
					}
					record["object_type"] = "expired"
					if variant == "expired-compact" || variant == "expired-retained" || strings.HasPrefix(variant, "expired-compact-") {
						delete(record, "scope")
						delete(record, "key")
					}
					switch variant {
					case "expired-wrong-id":
						record["object_id"] = parent.ID.String()
					case "expired-wrong-scope":
						record["scope"] = "execution:children:parent:" + child.ID.String()
					case "expired-wrong-key":
						record["key"] = parent.ID.String()
					case "expired-null-scope-only", "expired-empty-scope-only":
						delete(record, "key")
						record["scope"] = nil
						if variant == "expired-empty-scope-only" {
							record["scope"] = ""
						}
					case "expired-null-key-only", "expired-empty-key-only":
						delete(record, "scope")
						record["key"] = nil
						if variant == "expired-empty-key-only" {
							record["key"] = ""
						}
					case "expired-scope-only":
						delete(record, "key")
					case "expired-key-only":
						delete(record, "scope")
					case "expired-scope-null":
						record["scope"] = nil
					case "expired-key-null":
						record["key"] = nil
					case "expired-both-null":
						record["scope"], record["key"] = nil, nil
					case "expired-scope-empty":
						record["scope"] = ""
					case "expired-key-empty":
						record["key"] = ""
					case "expired-both-empty":
						record["scope"], record["key"] = "", ""
					case "expired-missing-digest", "expired-compact-missing-digest":
						delete(record, "input_digest")
					case "expired-empty-digest", "expired-compact-empty-digest":
						record["input_digest"] = ""
					case "expired-null-digest", "expired-compact-null-digest":
						record["input_digest"] = nil
					case "expired-short-digest":
						record["input_digest"] = "sha256:abcd"
					case "expired-uppercase-digest":
						record["input_digest"] = "sha256:" + strings.Repeat("A", 64)
					case "expired-unprefixed-digest":
						record["input_digest"] = strings.Repeat("a", 64)
					case "expired-nonstring-digest":
						record["input_digest"] = 42
					case "expired-invalid-digest", "expired-compact-invalid-digest":
						record["input_digest"] = "sha256:" + strings.Repeat("g", 64)
					case "expired-duplicate-extension":
						record["extension"] = json.RawMessage(`{"x":null,"x":1}`)
					case "expired-compact-forward", "expired-full-forward":
						record["extension"] = map[string]any{"items": []any{1, "unique", nil}}
					}
					raw, err := json.Marshal(record)
					if err != nil {
						return err
					}
					var duplicate string
					switch variant {
					case "expired-duplicate-scope":
						duplicate = `"scope":null,`
					case "expired-duplicate-key":
						duplicate = `"key":null,`
					case "expired-duplicate-digest", "expired-compact-duplicate-digest":
						duplicate = `"input_digest":null,`
					case "expired-duplicate-type":
						duplicate = `"object_type":"execution",`
					case "expired-duplicate-id":
						duplicate = `"object_id":null,`
					case "expired-duplicate-scope-case":
						duplicate = `"Scope":null,`
					case "expired-duplicate-scope-escape":
						duplicate = `"\u0073cope":null,`
					case "expired-duplicate-scope-unicode":
						duplicate = `"ſcope":null,`
					case "expired-duplicate-unknown":
						duplicate = `"extension":0,"extension":1,`
					}
					if duplicate != "" {
						raw = append([]byte("{"+duplicate), raw[1:]...)
					}
					return tx.Bucket([]byte("idempotency")).Put([]byte(scope+"\x00"+child.ID.String()), raw)
				}
				return nil
			})
			closeErr := db.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("fixture: %v %v", err, closeErr)
			}
			before, err := os.ReadFile(f.journal)
			if err != nil {
				t.Fatal(err)
			}
			// The existing authoritative query distinguishes incomplete membership from
			// valid expiration. Inbox must preserve that distinction through its shortcut.
			reader, err := store.Open(f.journal, store.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			children, childErr := reader.Children(ctx, store.ChildrenScope{Parent: &parent.ID}, time.Now().UTC())
			reader.Close()
			settled := variant == "retained-collected" || variant == "expired-compact" || variant == "expired-full" || variant == "expired-compact-forward" || variant == "expired-full-forward"
			incomplete := !settled && variant != "retained-uncollected"
			if strings.HasPrefix(variant, "expired-") && !settled && !errors.Is(childErr, store.ErrCorrupt) {
				t.Errorf("malformed expiry must be corruption: %v", childErr)
			}
			if incomplete {
				if !errors.Is(childErr, store.ErrChildrenBounded) && !errors.Is(childErr, store.ErrCorrupt) {
					t.Errorf("query failed to expose missing membership: %+v %v", children, childErr)
				}
			} else if childErr != nil {
				t.Fatal(childErr)
			}
			out.Reset()
			stderr.Reset()
			a.getenv = func(string) string { return "" }
			a.updateNotice = func(context.Context, string, common) *output.Warning {
				t.Fatal("inbox invoked maintenance")
				return nil
			}
			code := a.run(ctx, []string{"--journal", f.journal, "--config", f.config, "inbox", "--json", "--limit", "200"})
			var doc struct {
				OK     bool `json:"ok"`
				Result struct {
					Executions []inboxExecution `json:"executions"`
					Total      int              `json:"total"`
				} `json:"result"`
			}
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if code != 0 || !doc.OK {
				t.Fatalf("inbox failed instead of returning actionable coverage: %d %s", code, out.String())
			}
			var item *inboxExecution
			for i := range doc.Result.Executions {
				if doc.Result.Executions[i].ID == parent.ID {
					item = &doc.Result.Executions[i]
				}
			}
			if settled {
				if item != nil || doc.Result.Total != 0 {
					t.Fatalf("settled membership remained actionable: %s", out.String())
				}
			} else {
				if item == nil || item.ChildExitGuard == nil {
					t.Fatalf("incomplete retained membership falsely settled: %s", out.String())
				}
				if incomplete {
					if item.ChildExitGuard.CheckError != "children_query_incomplete" || item.WorkHealth != "child_collection_required" {
						t.Fatalf("wrong incomplete guard: %+v", item)
					}
					reason, action := false, false
					for _, r := range item.Reasons {
						reason = reason || r.Code == "children_query_incomplete"
					}
					for _, next := range item.NextActions {
						if next.SideEffectClass == output.ReadOnly && strings.Contains(strings.Join(next.Argv, " "), "children --execution "+parent.ID.String()) {
							action = true
						}
					}
					if !reason || !action {
						t.Fatalf("missing incomplete reason/read-only recheck action: %+v", item)
					}
					if variant == "missing-covered-exit" && len(item.UncollectedChildren) != 0 {
						t.Fatal("unknown membership invented uncollected rows")
					}
				} else if item.ChildExitGuard.CheckError != "" || len(item.UncollectedChildren) != 1 || item.UncollectedChildren[0].ID != child.ID || item.UncollectedChildren[0].State != model.StateCompleted {
					t.Fatalf("retained obligation changed: %+v", item)
				}
			}
			after, err := os.ReadFile(f.journal)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("read-only queries changed complete journal bytes: %v", err)
			}
		})
	}
}
