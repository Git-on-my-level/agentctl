package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

const callerBindingScope = "execution:caller-binding"

// Like delegation bindings, caller declarations have an immutable copy in the
// existing execution-owned idempotency graph. Older supervisors may discard
// fields unknown to their Execution struct when rewriting its projection, but
// never rewrite this snapshot. Older and newer cleanup remove it by ObjectID.
// An undeclared historical caller has no snapshot and remains unknown.
func putCallerBinding(tx *bbolt.Tx, execution model.Execution) error {
	if execution.Caller == nil {
		return nil
	}
	record := mutationRecord{Scope: callerBindingScope, Key: execution.ID.String(), InputDigest: callerBindingDigest(execution.Caller), ObjectType: "execution", ObjectID: execution.ID.String(), Caller: execution.Caller}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tx.Bucket(bIdempotency).Put([]byte(callerBindingScope+"\x00"+execution.ID.String()), body)
}

func hydrateCallerBinding(tx *bbolt.Tx, execution *model.Execution) error {
	data := tx.Bucket(bIdempotency).Get([]byte(callerBindingScope + "\x00" + execution.ID.String()))
	if data == nil {
		return nil
	}
	var record mutationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if record.Scope != callerBindingScope || record.Key != execution.ID.String() || record.ObjectType != "execution" || record.ObjectID != execution.ID.String() || record.Caller == nil {
		return fmt.Errorf("%w: invalid caller idempotency binding", ErrCorrupt)
	}
	if err := record.Caller.Validate(); err != nil || record.InputDigest != callerBindingDigest(record.Caller) {
		return fmt.Errorf("%w: invalid caller metadata", ErrCorrupt)
	}
	if execution.Caller != nil && *execution.Caller != *record.Caller {
		return fmt.Errorf("%w: conflicting caller bindings", ErrCorrupt)
	}
	execution.Caller = record.Caller
	return nil
}

func callerBindingDigest(caller *model.ExecutionCaller) string {
	// Both values are strict enums without NUL bytes. This private snapshot
	// digest is independent of launch semantic keys and contains no session data.
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(string(caller.Harness)+"\x00"+string(caller.Provenance))))
}
