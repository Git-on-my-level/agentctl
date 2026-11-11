package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

// ErrChildrenBounded means no complete answer was produced. Consumers must
// treat it as unknown, never as an empty list that permits a parent to exit.
var ErrChildrenBounded = errors.New("children query exceeded safety bound")

type ChildrenScope struct {
	Parent                    *ids.ExecutionID
	Provider, NativeSessionID string
}

type ChildExecution struct {
	ID           ids.ExecutionID             `json:"id"`
	CreatedAt    time.Time                   `json:"created_at"`
	State        model.State                 `json:"state"`
	Authority    model.Authority             `json:"authority"`
	Adapter      string                      `json:"adapter"`
	Remote       *model.DelegationRemotePlan `json:"remote,omitempty"`
	Reason       string                      `json:"reason"`
	WaiterStatus string                      `json:"waiter_status"`
	CollectArgv  []string                    `json:"collect_argv"`
}

// Children only visits caller-indexed projections, attribution snapshots,
// selected acknowledgements and bounded subscription metadata. Event/result history is
// never scanned. Each page drops the shared flock so writers can progress.
// Exact-ID awaits supply process-held lock evidence; unknown coverage stays actionable.
func (c Cleanup) Children(ctx context.Context, scope ChildrenScope, now time.Time) ([]ChildExecution, error) {
	if scope.Parent == nil && (scope.Provider == "" || scope.NativeSessionID == "") {
		return nil, ErrChildrenBounded
	}
	r := c.reader(ctx)
	var subscriptions []SubscriptionRecord
	if err := r.view(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bSubscriptions)
		if b == nil {
			return nil
		}
		size, records := 0, 0
		return b.ForEach(func(_, raw []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			size += len(raw)
			records++
			if records > 1024 || size > 4<<20 {
				return ErrChildrenBounded
			}
			var record SubscriptionRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return corrupt(err)
			}
			if fanoutUnderstands(raw, record.Subscription) {
				subscriptions = append(subscriptions, record)
			}
			return nil
		})
	}); err != nil {
		return nil, err
	}
	result := []ChildExecution{}
	prefix := childrenScopePrefix(scope)
	var after []byte
	scanned, totalBytes := 0, 0
	for {
		var last []byte
		err := r.view(func(tx *bbolt.Tx) error {
			cursor := tx.Bucket(bIdempotency).Cursor()
			key, raw := cursor.Seek(prefix)
			if after != nil {
				key, raw = cursor.Seek(after)
				if bytes.Equal(key, after) {
					key, raw = cursor.Next()
				}
			}
			size, count := 0, 0
			for ; key != nil && bytes.HasPrefix(key, prefix) && count < cleanupPageSize && size < cleanupPageBytes; key, raw = cursor.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				scanned++
				count++
				size += len(raw)
				totalBytes += len(raw)
				if scanned > 100000 || totalBytes > 64<<20 || len(raw) > 1<<20 {
					return ErrChildrenBounded
				}
				last = bytes.Clone(key)
				var index mutationRecord
				if err := json.Unmarshal(raw, &index); err != nil {
					return corrupt(err)
				}
				// An older manual cleanup may retain an expired-key tombstone.
				if index.ObjectType == expiredExecutionObject {
					continue
				}
				id, err := ids.ParseExecutionID(string(key[len(prefix):]))
				if err != nil || index.Scope != string(prefix[:len(prefix)-1]) || index.Key != id.String() || index.ObjectType != "execution" || index.ObjectID != id.String() {
					return corrupt(errors.New("invalid children index"))
				}
				executionRaw := tx.Bucket(bExecutions).Get([]byte(id.String()))
				if executionRaw == nil || len(executionRaw) > 1<<20 {
					return ErrChildrenBounded
				}
				var e model.Execution
				if err := decodeExecution(tx, executionRaw, &e); err != nil {
					return corrupt(err)
				}
				if err := e.Validate(); err != nil {
					return corrupt(err)
				}
				// Never trust an index to widen caller ownership.
				if scope.Parent != nil {
					if e.ParentExecutionID == nil || *e.ParentExecutionID != *scope.Parent {
						return corrupt(errors.New("children parent index mismatch"))
					}
				} else if e.CallerIdentity == nil || e.CallerIdentity.Provider != scope.Provider || e.CallerIdentity.NativeSessionID != scope.NativeSessionID {
					return corrupt(errors.New("children session index mismatch"))
				}
				if refusedAdmission(e) {
					continue
				}
				if e.State.Terminal() {
					// Attributed rows are new and do not need the historical ack epoch.
					if b := tx.Bucket(bAcknowledgements); b != nil && b.Get([]byte(e.ID.String())) != nil {
						continue
					}
				}
				item := ChildExecution{ID: e.ID, CreatedAt: e.CreatedAt, State: e.State, Authority: e.Authority, Adapter: e.Adapter, Reason: "running_without_known_waiter", WaiterStatus: "unknown", CollectArgv: []string{"agentctl", "await", e.ID.String(), "--content"}}
				if e.RemoteForwarded() {
					item.Remote = e.Delegation.RemotePlan
				}
				if e.State.Terminal() {
					item.Reason = "terminal_uncollected"
					item.WaiterStatus = "not_applicable"
				} else if !e.RemoteForwarded() {
					// Attention needs an attention wake; a terminal-only waiter cannot
					// unblock permission/input. A live await is not coverage for
					// conflicted evidence: it exits immediately on integrity conflicts.
					covered := false
					state, kind := model.StateCompleted, model.EventTerminal
					if e.State == model.StateAttention {
						state, kind = e.State, model.EventAttention
					} else if e.Observation.Integrity != model.IntegrityConflicted {
						var err error
						covered, err = hasLiveWaiter(ctx, r.path, e.ID)
						if err != nil {
							return err
						}
					}
					event := &model.Event{ExecutionID: e.ID, Authority: e.Authority, Kind: kind, State: &state}
					for _, record := range subscriptions {
						if matchesEvent(record.Subscription, event, now, func() []string { return e.Labels }) {
							covered = true
							break
						}
					}

					if covered {
						continue
					}
				}
				if len(result) >= 200 {
					return ErrChildrenBounded
				}
				result = append(result, item)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if last == nil {
			return result, nil
		}
		after = last
	}
}
