package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
	"go.etcd.io/bbolt"
)

func TestChildrenSubscriptionEvidenceReadOnlyCLI(t *testing.T) {
	for _, fault := range []string{"invalid-id", "wrong-valid-id", "invalid-key", "malformed-json", "malformed-record", "unrelated-invalid-id", "inactive-malformed-json", "valid", "inactive-invalid-id", "unknown-filter", "unknown-filter-invalid-id"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
			j, err := store.Open(path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = j.Close() })
			parent := identityFixture(t, j, "codex", "")
			child := parent
			child.ID, child.ParentExecutionID = "", &parent.ID
			child.State, child.Liveness, child.TerminalAt = model.StateRunning, model.LivenessAlive, nil
			caller := sessionIdentity("codex", "subscription-corruption-fixture", "explicit", "self_reported")
			child.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: *caller.ID, Provenance: "explicit"}
			child, _, err = j.CreateExecution(ctx, child, contracts.MutationKey{})
			if err != nil {
				t.Fatal(err)
			}
			value := subscription.Subscription{
				Origin: child.OriginHostID.String(), State: subscription.StateActive,
				Filter:      subscription.EventFilter{ExecutionIDs: []string{child.ID.String()}, Kinds: []string{"terminal"}},
				Destination: callback.Destination{Kind: callback.DestinationStdout},
			}
			value, _, err = j.PutSubscription(ctx, value)
			if err != nil {
				t.Fatal(err)
			}
			key := []byte(value.ID)
			if fault == "unrelated-invalid-id" || fault == "inactive-malformed-json" || fault == "inactive-invalid-id" || fault == "unknown-filter-invalid-id" {
				known := value
				known.ID = ""
				if _, _, err := j.PutSubscription(ctx, known); err != nil {
					t.Fatal(err)
				}
			}
			switch fault {
			case "invalid-id", "unrelated-invalid-id", "inactive-invalid-id", "unknown-filter-invalid-id":
				value.ID = "invalid-subscription"
			case "wrong-valid-id":
				id, err := ids.FromPayload(ids.TypeSubscription, 999)
				if err != nil {
					t.Fatal(err)
				}
				value.ID = id.String()
			}
			if fault == "unrelated-invalid-id" {
				value.Filter.ExecutionIDs = []string{parent.ID.String()}
			}
			if fault == "inactive-invalid-id" || fault == "inactive-malformed-json" {
				value.State = subscription.StatePaused
			}
			raw, err := json.Marshal(store.SubscriptionRecord{Subscription: value, Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "malformed-json", "inactive-malformed-json":
				raw = []byte(`{"subscription":{"state":"paused"},`)
			case "malformed-record":
				raw = []byte(`{"subscription":[]}`)
			case "unknown-filter", "unknown-filter-invalid-id":
				raw = bytes.Replace(raw, []byte(`"filter":{`), []byte(`"filter":{"future_scope":"narrow",`), 1)
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 50 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bbolt.Tx) error {
				b := tx.Bucket([]byte("subscriptions_v1"))
				if fault == "invalid-key" {
					if err := b.Delete(key); err != nil {
						return err
					}
					key = []byte("invalid-key")
				}
				return b.Put(key, raw)
			})
			closeErr := db.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("fixture: %v %v", err, closeErr)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, scope := range [][]string{{"--execution", parent.ID.String()}, {"--provider", "codex", "--native-session-id", "subscription-corruption-fixture"}} {
				var stdout, stderr bytes.Buffer
				a := testApp(&stdout, &stderr)
				a.getenv = func(string) string { return "" }
				a.updateNotice = func(context.Context, string, common) *output.Warning {
					t.Fatal("children triggered maintenance")
					return nil
				}
				code := a.run(ctx, append([]string{"--journal", path, "children", "--json"}, scope...))
				switch fault {
				case "valid", "inactive-invalid-id", "unknown-filter", "unknown-filter-invalid-id":
					var doc struct {
						OK     bool `json:"ok"`
						Result struct {
							Complete bool `json:"complete"`
							Count    int  `json:"count"`
						} `json:"result"`
					}
					wantCount := 0
					if fault == "unknown-filter" {
						wantCount = 1
					}
					if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil || code != 0 || !doc.OK || !doc.Result.Complete || doc.Result.Count != wantCount {
						t.Errorf("scope=%v exit=%d error=%v output=%s", scope, code, err, stdout.String())
					}
				default:
					var doc output.ErrorDocument
					if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil || code != 70 || doc.OK || doc.Error == nil || doc.Error.Code != output.CodeInternal || bytes.Contains(stdout.Bytes(), []byte(`"complete":true`)) {
						t.Errorf("scope=%v exit=%d error=%v output=%s", scope, code, err, stdout.String())
					}
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("CLI query changed journal bytes: %v", err)
			}
		})
	}
}
