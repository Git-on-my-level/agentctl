package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

// Every case crosses the real 128-entry page boundary. Mutations happen after
// the first page's handle closes, before opening page two; no sleeps or races
// against scheduling are needed. The inserted ID sorts before every scanned ID.
func TestChildrenPagedMembershipMutation(t *testing.T) {
	for _, parentScope := range []bool{true, false} {
		scopeName := map[bool]string{true: "parent", false: "caller-session"}[parentScope]
		for _, change := range []string{"insert", "final-insert", "final-delete", "legacy-journal-outcome", "legacy-scope-empty-aba", "legacy-insert", "legacy-delete", "legacy-tombstone", "legacy-rewrite", "legacy-insert-then-current-outcome", "delete", "outcome", "other-scope-admission", "legacy-outcome", "legacy-outcome-then-current-outcome"} {
			t.Run(scopeName+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				j, _, now := openTestJournal(t)
				parent, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
				if err != nil {
					t.Fatal(err)
				}
				caller := &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "explicit"}
				ordered := make([]string, 130)
				for i := range ordered {
					ordered[i] = durableTestID(ids.TypeExecution, uint64(1000+i))
				}
				sort.Strings(ordered)
				var first model.Execution
				for _, name := range ordered[1:] {
					e := sampleExecution(now)
					e.ID, _ = ids.ParseExecutionID(name)
					e.ParentExecutionID, e.CallerIdentity = &parent.ID, caller
					e, _, err = j.CreateExecution(ctx, e, contracts.MutationKey{})
					if err != nil {
						t.Fatal(err)
					}
					if first.ID.IsZero() {
						first = e
					}
				}
				low := first
				low.ID, _ = ids.ParseExecutionID(ordered[0])
				scope := ChildrenScope{Parent: &parent.ID}
				if !parentScope {
					scope = ChildrenScope{Provider: caller.Provider, NativeSessionID: caller.NativeSessionID}
				}
				prefix := childrenScopePrefix(scope)
				if change == "legacy-journal-outcome" || change == "legacy-scope-empty-aba" {
					if err := j.db.Update(func(tx *bbolt.Tx) error {
						if tx.Bucket([]byte("children_membership_generations")) != nil {
							if err := tx.DeleteBucket([]byte("children_membership_generations")); err != nil {
								return err
							}
						}
						if err := tx.Bucket(bMetadata).Delete([]byte("children_membership_writer")); err != nil {
							return err
						}
						return tx.Bucket(bMetadata).Delete([]byte("children_membership_pruned"))
					}); err != nil {
						t.Fatal(err)
					}
				}
				path := j.path
				if err := j.Close(); err != nil {
					t.Fatal(err)
				}
				opens := 0
				reader := Cleanup{Open: func(ctx context.Context, ro bool) (*Journal, error) {
					if !ro {
						t.Fatal("children requested writable handle")
					}
					opens++
					// Open 1 loads subscriptions; open 2 reads the first page.
					mutationOpen := 3
					if change == "final-insert" || change == "final-delete" {
						mutationOpen = 4
					}
					if opens == mutationOpen {
						writer, err := Open(path, Options{})
						if err != nil {
							t.Fatal(err)
						}
						outcome := func() error {
							next, err := writer.GetExecution(ctx, first.ID)
							if err != nil {
								return err
							}
							next.UpdatedAt = now.Add(time.Second)
							_, err = writer.UpdateExecution(ctx, next, next.Revision)
							return err
						}
						switch change {
						case "insert", "final-insert":
							_, _, err = writer.CreateExecution(ctx, low, contracts.MutationKey{})
						case "other-scope-admission":
							low.ParentExecutionID = nil
							low.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('b'), Provenance: "explicit"}
							_, _, err = writer.CreateExecution(ctx, low, contracts.MutationKey{})
						case "delete", "final-delete":
							err = writer.ClearMutation(ctx, contracts.MutationKey{Scope: string(prefix[:len(prefix)-1]), Key: first.ID.String(), InputDigest: attributionDigest(&launchAttribution{first.ParentExecutionID, first.CallerIdentity})})
						case "legacy-scope-empty-aba":
							for _, name := range ordered[1:] {
								err = writer.ClearMutation(ctx, contracts.MutationKey{Scope: string(prefix[:len(prefix)-1]), Key: name, InputDigest: attributionDigest(&launchAttribution{first.ParentExecutionID, first.CallerIdentity})})
								if err != nil {
									break
								}
							}
						case "outcome", "legacy-journal-outcome":
							err = outcome()
						default:
							// Raw db.Update deliberately models a pre-certificate binary:
							// it knows the existing indexes but not the new stamps.
							err = writer.db.Update(func(tx *bbolt.Tx) error {
								if change == "legacy-insert" || change == "legacy-insert-then-current-outcome" {
									return legacyInsertAttributedChild(tx, low)
								}
								if change == "legacy-outcome" || change == "legacy-outcome-then-current-outcome" {
									raw, _ := json.Marshal(first)
									return tx.Bucket(bExecutions).Put([]byte(first.ID.String()), raw)
								}
								key := append(bytes.Clone(prefix), []byte(first.ID.String())...)
								bucket := tx.Bucket(bIdempotency)
								if change == "legacy-delete" {
									return bucket.Delete(key)
								}
								raw := bytes.Clone(bucket.Get(key))
								if change == "legacy-tombstone" {
									var record mutationRecord
									if err := json.Unmarshal(raw, &record); err != nil {
										return err
									}
									record.ObjectType = expiredExecutionObject
									raw, _ = json.Marshal(record)
								} else {
									raw = append(raw, ' ')
								}
								return bucket.Put(key, raw)
							})
							if err == nil && (change == "legacy-insert-then-current-outcome" || change == "legacy-outcome-then-current-outcome") {
								err = outcome()
							}
						}
						if err != nil {
							t.Fatal(err)
						}
						if err := writer.Close(); err != nil {
							t.Fatal(err)
						}
					}
					return Open(path, Options{ReadOnly: true, LockTimeout: 50 * time.Millisecond})
				}}
				got, err := reader.Children(ctx, scope, now)
				if change == "outcome" || change == "legacy-journal-outcome" || change == "other-scope-admission" {
					if err != nil || len(got) != 129 {
						t.Fatalf("unrelated write invalidated membership: count=%d error=%v", len(got), err)
					}
				} else if !errors.Is(err, ErrChildrenBounded) || got != nil {
					t.Fatalf("mutation returned partial success: count=%d error=%v", len(got), err)
				}
				if opens < 3 {
					t.Fatal("probe did not cross the page boundary")
				}
				// A retry after the mutation must accept the now-stable index,
				// including legacy stamps or the persisted legacy-write fence.
				stable, err := cleanupForPath(path).Children(ctx, scope, now)
				if err != nil {
					t.Fatalf("stable retry: %v", err)
				}
				want := 129
				if change == "insert" || change == "final-insert" || change == "legacy-insert" || change == "legacy-insert-then-current-outcome" {
					want = 130
				}
				if change == "delete" || change == "final-delete" || change == "legacy-delete" || change == "legacy-tombstone" {
					want = 128
				}
				if change == "legacy-scope-empty-aba" {
					want = 0
				}
				if len(stable) != want {
					t.Fatalf("stable retry count=%d want=%d", len(stable), want)
				}
			})
		}
	}
}

func legacyInsertAttributedChild(tx *bbolt.Tx, e model.Execution) error {
	raw, _ := json.Marshal(e)
	if err := tx.Bucket(bExecutions).Put([]byte(e.ID.String()), raw); err != nil {
		return err
	}
	v := &launchAttribution{e.ParentExecutionID, e.CallerIdentity}
	record := mutationRecord{Scope: attributionBindingScope, Key: e.ID.String(), InputDigest: attributionDigest(v), ObjectType: "execution", ObjectID: e.ID.String(), Attribution: v}
	raw, _ = json.Marshal(record)
	bucket := tx.Bucket(bIdempotency)
	if err := bucket.Put([]byte(attributionBindingScope+"\x00"+e.ID.String()), raw); err != nil {
		return err
	}
	for _, scope := range []ChildrenScope{{Parent: e.ParentExecutionID}, {Provider: e.CallerIdentity.Provider, NativeSessionID: e.CallerIdentity.NativeSessionID}} {
		prefix := childrenScopePrefix(scope)
		record.Scope, record.Attribution = string(prefix[:len(prefix)-1]), nil
		raw, _ = json.Marshal(record)
		if err := bucket.Put(append(prefix, []byte(e.ID.String())...), raw); err != nil {
			return err
		}
	}
	return nil
}

func TestChildrenPagedMembershipCleanup(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	caller := &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "explicit"}
	ordered := make([]string, 129)
	for i := range ordered {
		ordered[i] = durableTestID(ids.TypeExecution, uint64(1000+i))
	}
	sort.Strings(ordered)
	for i, name := range ordered {
		e := sampleExecution(now)
		e.ID, _ = ids.ParseExecutionID(name)
		e.CallerIdentity = caller
		if i == 0 {
			e.State, e.Liveness, e.TerminalAt = model.StateCompleted, model.LivenessExited, &now
		}
		e, _, err := j.CreateExecution(ctx, e, contracts.MutationKey{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementAwait); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := j.path
	j.Close()
	opens := 0
	reader := Cleanup{Open: func(ctx context.Context, ro bool) (*Journal, error) {
		if !ro {
			t.Fatal("query requested write lock")
		}
		opens++
		if opens == 3 {
			progress, err := cleanupForPath(path).Maintain(ctx, now.Add(time.Hour))
			if err != nil || progress.Executions != 1 {
				t.Fatalf("cleanup=%+v error=%v", progress, err)
			}
		}
		return Open(path, Options{ReadOnly: true, LockTimeout: 50 * time.Millisecond})
	}}
	scope := ChildrenScope{Provider: caller.Provider, NativeSessionID: caller.NativeSessionID}
	if got, err := reader.Children(ctx, scope, now); !errors.Is(err, ErrChildrenBounded) || got != nil {
		t.Fatalf("cleanup during scan returned partial success: count=%d error=%v", len(got), err)
	}
	if got, err := cleanupForPath(path).Children(ctx, scope, now); err != nil || len(got) != 128 {
		t.Fatalf("stable retry count=%d error=%v", len(got), err)
	}
}

func TestChildrenMembershipReclaimsEmptyScope(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := sampleExecution(now)
	e.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "explicit"}
	e.State, e.Liveness, e.TerminalAt = model.StateCompleted, model.LivenessExited, &now
	e, _, err := j.CreateExecution(ctx, e, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementAwait); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	progress, err := cleanupForPath(path).Maintain(ctx, now.Add(time.Hour))
	if err != nil || progress.Executions != 1 {
		t.Fatalf("cleanup=%+v error=%v", progress, err)
	}
	read, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if err := read.db.View(func(tx *bbolt.Tx) error {
		if stamps := tx.Bucket([]byte("children_membership_generations")); stamps != nil {
			key, _ := stamps.Cursor().First()
			if key != nil {
				t.Fatal("empty scope metadata survived cleanup")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
