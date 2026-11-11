package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

// Keep this separate from launch-attribution: older readers validate that
// snapshot's digest using its old shape. Extending it would break mixed-version
// reads. This execution-owned record is opaque to older readers and cleanup.
const automaticContinuationScope = "execution:automatic-continuation"

func automaticContinuationDigest(source ids.ExecutionID) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(source.String())))
}
func putAutomaticContinuationBinding(tx *bbolt.Tx, execution model.Execution) error {
	if execution.AutoContinuationOf == nil {
		return nil
	}
	record := mutationRecord{Scope: automaticContinuationScope, Key: execution.ID.String(), ObjectType: "execution", ObjectID: execution.ID.String(), InputDigest: automaticContinuationDigest(*execution.AutoContinuationOf), AutomaticContinuationOf: execution.AutoContinuationOf}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tx.Bucket(bIdempotency).Put([]byte(automaticContinuationScope+"\x00"+execution.ID.String()), raw)
}
func hydrateAutomaticContinuationBinding(tx *bbolt.Tx, execution *model.Execution) error {
	raw := tx.Bucket(bIdempotency).Get([]byte(automaticContinuationScope + "\x00" + execution.ID.String()))
	if raw == nil {
		return nil
	}
	var record mutationRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return err
	}
	if record.Scope != automaticContinuationScope || record.Key != execution.ID.String() || record.ObjectType != "execution" || record.ObjectID != execution.ID.String() || record.AutomaticContinuationOf == nil || record.InputDigest != automaticContinuationDigest(*record.AutomaticContinuationOf) {
		return fmt.Errorf("%w: invalid automatic continuation binding", ErrCorrupt)
	}
	source := *record.AutomaticContinuationOf
	if _, err := ids.ParseExecutionID(source.String()); err != nil || source == execution.ID {
		return fmt.Errorf("%w: invalid automatic continuation source", ErrCorrupt)
	}
	if execution.AutoContinuationOf != nil && *execution.AutoContinuationOf != source {
		return fmt.Errorf("%w: conflicting automatic continuation source", ErrCorrupt)
	}
	execution.AutoContinuationOf = record.AutomaticContinuationOf
	return nil
}
