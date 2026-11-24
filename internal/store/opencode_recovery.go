package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

// Admission and the back-pointer commit together. An owner lost after this
// transaction leaves the exact admitted successor observable, even if the
// native prompt was never sent.
func (j *Journal) linkOpenCodeRecovery(tx *bbolt.Tx, successor model.Execution, now time.Time) error {
	bucket := tx.Bucket(bExecutions)
	key := []byte(successor.RetryOf.String())
	raw := bucket.Get(key)
	if raw == nil {
		return ErrNotFound
	}
	var source model.Execution
	if err := decodeExecution(tx, raw, &source); err != nil {
		return corrupt(err)
	}
	recovery := source.OpenCodeRecovery
	if !source.OpenCodeRecoveryPending(now) || !recovery.Claimed || now.Before(recovery.NotBefore) {
		return fmt.Errorf("%w: OpenCode recovery claim is no longer active", ErrConflict)
	}
	source.SupersededBy = &successor.ID
	source.Revision++
	source.UpdatedAt = now
	encoded, err := json.Marshal(source)
	if err != nil {
		return err
	}
	if err := bucket.Put(key, encoded); err != nil {
		return err
	}
	projection := map[string]any{"authority_scope": source.Authority, "execution_fingerprint": source.ID.String(),
		"kind": model.EventSuperseded, "source": "opencode-recovery:" + successor.ID.String(), "state": source.State}
	dedupe, canonical, err := callback.SemanticDedupeKey(source.Adapter, 1, projection)
	if err != nil {
		return err
	}
	event := model.Event{ExecutionID: source.ID, OriginHostID: source.OriginHostID, Ordering: model.OrderingObservation,
		Kind: model.EventSuperseded, State: &source.State, Authority: source.Authority, Adapter: source.Adapter,
		ObservedAt: now, DedupeKey: dedupe, DedupeVersion: 1,
		Payload: map[string]any{"superseded_by": successor.ID.String(), "relation": "opencode_recovery", "reason": recovery.Kind, "attempt": recovery.Attempt}}
	_, _, err = j.appendEventTx(tx, event, canonical)
	return err
}
