package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

func childrenCorruptionFixture(t *testing.T) (*Journal, model.Execution, []ChildrenScope, time.Time) {
	t.Helper()
	j, _, now := openTestJournal(t)
	parent, _, err := j.CreateExecution(context.Background(), sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	e := sampleExecution(now)
	e.ParentExecutionID = &parent.ID
	e.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "explicit"}
	e.State, e.Liveness, e.TerminalAt = model.StateCompleted, model.LivenessExited, &now
	e, _, err = j.CreateExecution(context.Background(), e, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	return j, e, []ChildrenScope{{Parent: &parent.ID}, {Provider: e.CallerIdentity.Provider, NativeSessionID: e.CallerIdentity.NativeSessionID}}, now
}

// Check both scopes against the same bytes, using fresh read-only handles for
// every page. Corruption must produce no partial result, panic or repair write.
func checkChildrenCorruptionRead(t *testing.T, j *Journal, scopes []ChildrenScope, now time.Time, wantErr error, wantCount int) {
	t.Helper()
	path := j.path
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader := Cleanup{Open: func(ctx context.Context, ro bool) (*Journal, error) {
		if !ro {
			t.Fatal("children requested write access")
		}
		return Open(path, Options{ReadOnly: true, LockTimeout: 50 * time.Millisecond})
	}}
	for i, scope := range scopes {
		got, err := reader.Children(context.Background(), scope, now)
		if wantErr != nil {
			if !errors.Is(err, wantErr) || got != nil {
				t.Errorf("scope %d: count=%d error=%v, want %v and no partial result", i, len(got), err, wantErr)
			}
		} else if err != nil || len(got) != wantCount {
			t.Errorf("scope %d: count=%d error=%v, want count=%d", i, len(got), err, wantCount)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read-only query changed journal bytes: %v", err)
	}
}

func TestChildrenCorruptAcknowledgement(t *testing.T) {
	for _, test := range []string{"malformed-json", "null", "empty-record", "wrong-id", "invalid-id", "invalid-source", "missing-source", "missing-time", "invalid-time", "nested-bucket", "oversized", "absent", "result", "await", "bulk_reconciled"} {
		t.Run(test, func(t *testing.T) {
			j, e, scopes, now := childrenCorruptionFixture(t)
			ack := ExecutionAcknowledgement{ExecutionID: e.ID, AcknowledgedAt: now, Source: AcknowledgementAwait}
			switch test {
			case "wrong-id":
				ack.ExecutionID, _ = ids.ParseExecutionID(durableTestID(ids.TypeExecution, 900))
			case "invalid-source":
				ack.Source = "unverified"
			case "missing-source":
				ack.Source = ""
			case "missing-time":
				ack.AcknowledgedAt = time.Time{}
			case "result", "await", "bulk_reconciled":
				ack.Source = test
			}
			raw, err := json.Marshal(ack)
			if err != nil {
				t.Fatal(err)
			}
			switch test {
			case "malformed-json":
				raw = []byte("not-json")
			case "null":
				raw = []byte("null")
			case "empty-record":
				raw = []byte("{}")
			case "invalid-id":
				raw = bytes.Replace(raw, []byte(e.ID.String()), []byte("invalid-execution"), 1)
			case "invalid-time":
				raw = bytes.Replace(raw, []byte(now.Format(time.RFC3339)), []byte("invalid-time"), 1)
			case "oversized":
				raw = append(raw, []byte(strings.Repeat(" ", 1<<20))...)
			}
			if test != "absent" {
				if err := j.db.Update(func(tx *bbolt.Tx) error {
					b := tx.Bucket(bAcknowledgements)
					if test == "nested-bucket" {
						_, err := b.CreateBucket([]byte(e.ID.String()))
						return err
					}
					return b.Put([]byte(e.ID.String()), raw)
				}); err != nil {
					t.Fatal(err)
				}
			}
			wantErr, wantCount := ErrCorrupt, 0
			if test == "oversized" {
				wantErr = ErrChildrenBounded
			}
			if test == "absent" || test == "result" || test == "await" || test == "bulk_reconciled" {
				wantErr = nil
				if test == "absent" {
					wantCount = 1
				}
			}
			checkChildrenCorruptionRead(t, j, scopes, now, wantErr, wantCount)
		})
	}
}

func TestChildrenCorruptTombstone(t *testing.T) {
	for _, test := range []string{"retained-compact", "retained-full", "missing-id", "wrong-id", "invalid-id", "invalid-key", "wrong-scope", "wrong-record-key", "nested-execution", "absent-compact", "absent-full"} {
		t.Run(test, func(t *testing.T) {
			j, e, scopes, now := childrenCorruptionFixture(t)
			if err := j.db.Update(func(tx *bbolt.Tx) error {
				if test != "retained-compact" && test != "retained-full" {
					if err := tx.Bucket(bExecutions).Delete([]byte(e.ID.String())); err != nil {
						return err
					}
				}
				if test == "nested-execution" {
					if _, err := tx.Bucket(bExecutions).CreateBucket([]byte(e.ID.String())); err != nil {
						return err
					}
				}
				for _, scope := range scopes {
					prefix := childrenScopePrefix(scope)
					index := mutationRecord{ObjectType: expiredExecutionObject, ObjectID: e.ID.String(), InputDigest: hash('b')}
					key := append(bytes.Clone(prefix), []byte(e.ID.String())...)
					switch test {
					case "retained-full", "absent-full":
						index.Scope, index.Key = string(prefix[:len(prefix)-1]), e.ID.String()
					case "missing-id":
						index.ObjectID = ""
					case "wrong-id":
						index.ObjectID = durableTestID(ids.TypeExecution, 900)
					case "invalid-id":
						index.ObjectID = "invalid-execution"
					case "invalid-key":
						key = append(bytes.Clone(prefix), []byte("invalid-execution")...)
						if err := tx.Bucket(bIdempotency).Delete(append(bytes.Clone(prefix), []byte(e.ID.String())...)); err != nil {
							return err
						}
					case "wrong-scope":
						index.Scope = "another-scope"
					case "wrong-record-key":
						index.Key = durableTestID(ids.TypeExecution, 900)
					}
					raw, err := json.Marshal(index)
					if err != nil {
						return err
					}
					if err := tx.Bucket(bIdempotency).Put(key, raw); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			wantErr := ErrCorrupt
			if test == "absent-compact" || test == "absent-full" {
				wantErr = nil
			}
			checkChildrenCorruptionRead(t, j, scopes, now, wantErr, 0)
		})
	}
}

func TestChildrenMissingRequiredBucket(t *testing.T) {
	for _, name := range [][]byte{bIdempotency, bExecutions} {
		t.Run(string(name), func(t *testing.T) {
			j, _, scopes, now := childrenCorruptionFixture(t)
			if err := j.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket(name) }); err != nil {
				t.Fatal(err)
			}
			checkChildrenCorruptionRead(t, j, scopes, now, ErrCorrupt, 0)
		})
	}
}

func TestChildrenOptionalHistoricalMetadataNeedsNoUpgrade(t *testing.T) {
	j, _, scopes, now := childrenCorruptionFixture(t)
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bAcknowledgements, bSubscriptions, bChildrenGenerations} {
			if tx.Bucket(name) != nil {
				if err := tx.DeleteBucket(name); err != nil {
					return err
				}
			}
		}
		if err := tx.Bucket(bMetadata).Delete(keyChildrenWriter); err != nil {
			return err
		}
		return tx.Bucket(bMetadata).Delete(keyChildrenPruned)
	}); err != nil {
		t.Fatal(err)
	}
	checkChildrenCorruptionRead(t, j, scopes, now, nil, 1)
}
