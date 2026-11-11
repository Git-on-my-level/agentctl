package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrReplayExpired = errors.New("execution replay expired")

// ReplayExpired refuses to relaunch work whose evidence was explicitly cleaned.
// The request key stays in bIdempotency; no prompt, plan or answer is retained.
type ReplayExpired struct{ ExecutionID string }

func (e *ReplayExpired) Error() string {
	return fmt.Sprintf("%v: %s was cleaned; refusing to relaunch this request key", ErrReplayExpired, e.ExecutionID)
}
func (e *ReplayExpired) Unwrap() error { return ErrReplayExpired }

const expiredExecutionObject = "expired"

func replayTombstone(raw []byte) ([]byte, error) {
	var record mutationRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, corrupt(err)
	}
	// These internal snapshots are not externally reusable request keys.
	if strings.HasPrefix(record.Scope, childrenIndexScope) || record.Scope == attributionBindingScope || record.Scope == callerBindingScope || record.Scope == delegationBindingScope {
		return nil, nil
	}
	return json.Marshal(struct {
		InputDigest string `json:"input_digest"`
		ObjectType  string `json:"object_type"`
		ObjectID    string `json:"object_id"`
	}{record.InputDigest, expiredExecutionObject, record.ObjectID})
}
