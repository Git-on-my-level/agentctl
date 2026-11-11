package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

const attributionBindingScope = "execution:launch-attribution"
const childrenIndexScope = "execution:children:"

func childrenScopePrefix(scope ChildrenScope) []byte {
	if scope.Parent != nil {
		return []byte(childrenIndexScope + "parent:" + scope.Parent.String() + "\x00")
	}
	return []byte(childrenIndexScope + "session:" + scope.Provider + ":" + scope.NativeSessionID + "\x00")
}

type launchAttribution struct {
	Parent *ids.ExecutionID      `json:"parent_execution_id,omitempty"`
	Caller *model.CallerIdentity `json:"caller_identity,omitempty"`
}

func attributionDigest(v *launchAttribution) string {
	raw, _ := json.Marshal(v)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}
func putAttributionBinding(tx *bbolt.Tx, e model.Execution) error {
	if e.ParentExecutionID == nil && e.CallerIdentity == nil {
		return nil
	}
	v := &launchAttribution{e.ParentExecutionID, e.CallerIdentity}
	record := mutationRecord{Scope: attributionBindingScope, Key: e.ID.String(), InputDigest: attributionDigest(v), ObjectType: "execution", ObjectID: e.ID.String(), Attribution: v}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	bucket := tx.Bucket(bIdempotency)
	if err := bucket.Put([]byte(attributionBindingScope+"\x00"+e.ID.String()), raw); err != nil {
		return err
	}
	// Index references live in the same execution-owned graph as immutable
	// snapshots. Older cleanup code already discovers them by ObjectID.
	scopes := []ChildrenScope{}
	if e.ParentExecutionID != nil {
		scopes = append(scopes, ChildrenScope{Parent: e.ParentExecutionID})
	}
	if e.CallerIdentity != nil {
		scopes = append(scopes, ChildrenScope{Provider: e.CallerIdentity.Provider, NativeSessionID: e.CallerIdentity.NativeSessionID})
	}
	for _, scope := range scopes {
		prefix := childrenScopePrefix(scope)
		index := mutationRecord{Scope: string(prefix[:len(prefix)-1]), Key: e.ID.String(), InputDigest: record.InputDigest, ObjectType: "execution", ObjectID: e.ID.String()}
		body, err := json.Marshal(index)
		if err != nil {
			return err
		}
		if err := bucket.Put(append(prefix, []byte(e.ID.String())...), body); err != nil {
			return err
		}
		if err := noteChildrenIndexMutation(tx, append(prefix, []byte(e.ID.String())...)); err != nil {
			return err
		}
	}
	return nil
}
func hydrateAttributionBinding(tx *bbolt.Tx, e *model.Execution) error {
	raw := tx.Bucket(bIdempotency).Get([]byte(attributionBindingScope + "\x00" + e.ID.String()))
	return hydrateAttributionRecord(raw, e)
}

// Shared by journal reads and retention's paged immutable-metadata snapshot.
// Hydration validates provenance without rewriting legacy projection bytes.
func hydrateAttributionRecord(raw []byte, e *model.Execution) error {
	if raw == nil {
		return nil
	}
	var r mutationRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if r.Scope != attributionBindingScope || r.Key != e.ID.String() || r.ObjectType != "execution" || r.ObjectID != e.ID.String() || r.Attribution == nil || r.InputDigest != attributionDigest(r.Attribution) {
		return fmt.Errorf("%w: invalid launch attribution binding", ErrCorrupt)
	}
	v := r.Attribution
	if v.Parent != nil {
		if _, err := ids.ParseExecutionID(v.Parent.String()); err != nil || *v.Parent == e.ID {
			return fmt.Errorf("%w: invalid parent execution", ErrCorrupt)
		}
	}
	if v.Caller != nil {
		if err := v.Caller.Validate(); err != nil {
			return corrupt(err)
		}
	}
	if (e.ParentExecutionID != nil && !reflect.DeepEqual(e.ParentExecutionID, v.Parent)) || (e.CallerIdentity != nil && !reflect.DeepEqual(e.CallerIdentity, v.Caller)) {
		return fmt.Errorf("%w: conflicting launch attribution", ErrCorrupt)
	}
	e.ParentExecutionID, e.CallerIdentity = v.Parent, v.Caller
	return nil
}
