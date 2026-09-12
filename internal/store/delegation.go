package store

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

const delegationBindingScope = "execution:delegation-binding"

// Keep a second immutable copy in the existing idempotency graph. Older
// supervisors can rewrite execution projections without knowing new fields;
// they do not rewrite immutable idempotency records. Existing retention removes
// this record with its execution, including on older binaries.
func putDelegationBinding(tx *bbolt.Tx, execution model.Execution) error {
	if execution.Delegation == nil {
		return nil
	}
	record := mutationRecord{Scope: delegationBindingScope, Key: execution.ID.String(), InputDigest: execution.Delegation.RequestSHA256, ObjectType: "execution", ObjectID: execution.ID.String(), Delegation: execution.Delegation, NativePlan: execution.Delegation.NativePlan}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tx.Bucket(bIdempotency).Put([]byte(delegationBindingScope+"\x00"+execution.ID.String()), body)
}

func decodeExecution(tx *bbolt.Tx, raw []byte, execution *model.Execution) error {
	if err := json.Unmarshal(raw, execution); err != nil {
		return err
	}
	data := tx.Bucket(bIdempotency).Get([]byte(delegationBindingScope + "\x00" + execution.ID.String()))
	if data == nil {
		return nil
	} // Legacy non-delegated records have no snapshot.
	var record mutationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if record.Scope != delegationBindingScope || record.Key != execution.ID.String() || record.ObjectType != "execution" || record.ObjectID != execution.ID.String() || record.Delegation == nil || record.InputDigest != record.Delegation.RequestSHA256 {
		return fmt.Errorf("%w: invalid delegation idempotency binding", ErrCorrupt)
	}
	if err := record.Delegation.Validate(); err != nil {
		return fmt.Errorf("%w: invalid delegation metadata", ErrCorrupt)
	}
	if execution.Delegation != nil && !reflect.DeepEqual(execution.Delegation, record.Delegation) {
		return fmt.Errorf("%w: conflicting delegation bindings", ErrCorrupt)
	}
	record.Delegation.NativePlan = record.NativePlan
	if err := record.Delegation.Validate(); err != nil {
		return fmt.Errorf("%w: invalid delegation native plan", ErrCorrupt)
	}
	execution.Delegation = record.Delegation
	return nil
}
