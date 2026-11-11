package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
	"go.etcd.io/bbolt"
)

func TestChildrenCorruptionReturnsStructuredFailureWithoutWrites(t *testing.T) {
	for _, fault := range []string{"acknowledgement", "tombstone", "missing-index"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
			j, err := store.Open(path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			parent := identityFixture(t, j, "codex", "")
			child := parent
			child.ID = ""
			child.ParentExecutionID = &parent.ID
			caller := sessionIdentity("codex", "corrupt-state-fixture", "explicit", "self_reported")
			child.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: *caller.ID, Provenance: "explicit"}
			now := time.Now().UTC()
			child.State, child.Liveness, child.TerminalAt = model.StateCompleted, model.LivenessExited, &now
			child, _, err = j.CreateExecution(ctx, child, contracts.MutationKey{})
			if err != nil {
				t.Fatal(err)
			}
			j.Close()
			db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 50 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bbolt.Tx) error {
				switch fault {
				case "acknowledgement":
					raw, err := json.Marshal(store.ExecutionAcknowledgement{ExecutionID: parent.ID, AcknowledgedAt: now, Source: "unverified"})
					if err != nil {
						return err
					}
					return tx.Bucket([]byte("execution_acknowledgements")).Put([]byte(child.ID.String()), raw)
				case "tombstone":
					raw, _ := json.Marshal(map[string]string{"object_type": "expired", "object_id": child.ID.String()})
					for _, prefix := range []string{"execution:children:parent:" + parent.ID.String(), "execution:children:session:codex:" + *caller.ID} {
						if err := tx.Bucket([]byte("idempotency")).Put([]byte(prefix+"\x00"+child.ID.String()), raw); err != nil {
							return err
						}
					}
					return nil
				default:
					return tx.DeleteBucket([]byte("idempotency"))
				}
			})
			closeErr := db.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("fixture: %v %v", err, closeErr)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, scope := range [][]string{{"--execution", parent.ID.String()}, {"--provider", "codex", "--native-session-id", "corrupt-state-fixture"}} {
				var stdout, stderr bytes.Buffer
				a := testApp(&stdout, &stderr)
				a.getenv = func(string) string { return "" }
				a.updateNotice = func(context.Context, string, common) *output.Warning {
					t.Fatal("corrupt query triggered maintenance")
					return nil
				}
				args := append([]string{"--journal", path, "children", "--json"}, scope...)
				code := a.run(ctx, args)
				var document output.ErrorDocument
				if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
					t.Fatalf("non-JSON error: %v %s", err, stdout.String())
				}
				wantCode, wantError := 70, output.CodeInternal
				if fault == "missing-index" && scope[0] == "--execution" {
					// Identity observation already treats unavailable evidence as
					// incomplete; preserve that convention before the store query.
					wantCode, wantError = 9, output.CodeTimeout
					if document.Error == nil || document.Error.Details["diagnostic_code"] != "children_query_incomplete" || document.Error.Details["complete"] != false {
						t.Fatalf("missing identity evidence lost incomplete diagnostic: %s", stdout.String())
					}
				}
				if code != wantCode || document.OK || document.Error == nil || document.Error.Code != wantError || bytes.Contains(stdout.Bytes(), []byte(`"complete":true`)) {
					t.Fatalf("scope=%v exit=%d output=%s", scope, code, stdout.String())
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("CLI query changed corrupt journal: %v", err)
			}
		})
	}
}
