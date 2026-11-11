package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

// Exercise real attribution and manual retention producers. Current cleanup
// removes internal membership indexes, so reinstall the produced compact replay
// record to model historical retained membership. The full compatibility form
// combines that expiry with the exact scope/key emitted by putAttributionBinding.
func TestChildrenExpiryProducerCompatibility(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "compact", true: "full"}[full], func(t *testing.T) {
			ctx := context.Background()
			j, _, now := openTestJournal(t)
			caller := &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "explicit"}
			e := sampleExecution(now)
			e.CallerIdentity = caller
			e.State, e.Liveness, e.TerminalAt = model.StateCompleted, model.LivenessExited, &now
			digest := attributionDigest(&launchAttribution{Caller: caller})
			request := contracts.MutationKey{Scope: "producer-expiry-test", Key: "request", InputDigest: digest}
			e, _, err := j.CreateExecution(ctx, e, request)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementAwait); err != nil {
				t.Fatal(err)
			}
			scope := ChildrenScope{Provider: caller.Provider, NativeSessionID: caller.NativeSessionID}
			indexKey := append(childrenScopePrefix(scope), []byte(e.ID.String())...)
			var producedIndex []byte
			if err := j.db.View(func(tx *bbolt.Tx) error {
				producedIndex = bytes.Clone(tx.Bucket(bIdempotency).Get(indexKey))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			path := j.path
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			cleanup := cleanupForPath(path)
			cutoff := now.Add(time.Hour)
			plan, err := cleanup.Plan(ctx, cutoff, CleanupOptions{})
			if err != nil || len(plan.Eligible) != 1 || plan.Records.ReplayTombstones != 1 {
				t.Fatalf("real retention plan: %+v %v", plan, err)
			}
			if _, err := cleanup.Apply(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); err != nil {
				t.Fatal(err)
			}
			j, err = Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := j.update(func(tx *bbolt.Tx) error {
				b := tx.Bucket(bIdempotency)
				compact := bytes.Clone(b.Get([]byte(request.Scope + "\x00" + request.Key)))
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(compact, &fields); err != nil {
					return err
				}
				if len(fields) != 3 || string(fields["input_digest"]) != `"`+digest+`"` || string(fields["object_id"]) != `"`+e.ID.String()+`"` || string(fields["object_type"]) != `"expired"` {
					t.Fatalf("unexpected actual compact producer output: %s", compact)
				}
				if tx.Bucket(bExecutions).Get([]byte(e.ID.String())) != nil || b.Get(indexKey) != nil {
					t.Fatal("real cleanup retained execution/internal membership")
				}
				expiry := compact
				if full {
					var metadata map[string]json.RawMessage
					if err := json.Unmarshal(producedIndex, &metadata); err != nil {
						return err
					}
					fields["scope"], fields["key"] = metadata["scope"], metadata["key"]
					var err error
					expiry, err = json.Marshal(fields)
					if err != nil {
						return err
					}
				}
				if err := b.Put(indexKey, expiry); err != nil {
					return err
				}
				return noteChildrenIndexMutation(tx, indexKey)
			}); err != nil {
				t.Fatal(err)
			}
			checkChildrenCorruptionRead(t, j, []ChildrenScope{scope}, now, nil, 0)
		})
	}
}
