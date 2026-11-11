package store

import (
	"encoding/json"
	"fmt"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

// A separate execution-owned binding keeps the historical recheck obligation
// through mixed-version projection rewrites without extending old hashed shapes.
// It is not a request-key replay promise or a new retention reference.
const childExitRecheckScope = "execution:child-exit-recheck"
const childExitRecheckDigest = "child-exit-recheck:v1"

func putChildExitRecheck(tx *bbolt.Tx, e model.Execution) error {
	if !e.ChildExitRecheck {
		return nil
	}
	r := mutationRecord{Scope: childExitRecheckScope, Key: e.ID.String(), ObjectType: "execution", ObjectID: e.ID.String(), InputDigest: childExitRecheckDigest}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return tx.Bucket(bIdempotency).Put([]byte(childExitRecheckScope+"\x00"+e.ID.String()), raw)
}
func hydrateChildExitRecheck(raw []byte, e *model.Execution) error {
	if raw == nil {
		return nil
	}
	var r mutationRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if r.Scope != childExitRecheckScope || r.Key != e.ID.String() || r.ObjectType != "execution" || r.ObjectID != e.ID.String() || r.InputDigest != childExitRecheckDigest {
		return fmt.Errorf("%w: invalid child-exit recheck binding", ErrCorrupt)
	}
	e.ChildExitRecheck = true
	return nil
}
