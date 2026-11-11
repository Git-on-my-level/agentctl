package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

const (
	AcknowledgementResult = "result"
	AcknowledgementAwait  = "await"
	// AcknowledgementBulk is an explicit operator stamp from reconcile --apply.
	// It records that collection was acknowledged without reading result content.
	AcknowledgementBulk = "bulk_reconciled"
)

// ExecutionAcknowledgement records that a caller collected a terminal
// execution. It is the direct-mode counterpart of subscription delivery
// acknowledgement: a small durable stamp, not an execution-envelope rewrite.
type ExecutionAcknowledgement struct {
	ExecutionID    ids.ExecutionID `json:"execution_id"`
	AcknowledgedAt time.Time       `json:"acknowledged_at"`
	Source         string          `json:"source"`
}

// AcknowledgementIndex is the read projection used by recent --unreconciled.
// A missing epoch means the journal predates this feature and has not yet been
// write-opened by a build that records acknowledgements; those terminals are
// treated as already reconciled so a first query does not flood with history.
type AcknowledgementIndex struct {
	Epoch time.Time
	ByID  map[ids.ExecutionID]ExecutionAcknowledgement
}

func (idx AcknowledgementIndex) Unreconciled(execution model.Execution) bool {
	if !execution.State.Terminal() || execution.TerminalAt == nil {
		return false
	}
	if _, ok := idx.ByID[execution.ID]; ok {
		return false
	}
	if idx.Epoch.IsZero() {
		return false
	}
	trackedAt := *execution.TerminalAt
	if execution.RemoteForwarded() {
		// terminal_at belongs to the remote clock. Local admission proves when
		// this journal began tracking the forwarding row, independent of skew.
		trackedAt = execution.CreatedAt
	}
	return !trackedAt.Before(idx.Epoch)
}

func (j *Journal) AcknowledgementIndex(ctx context.Context) (AcknowledgementIndex, error) {
	if err := ctx.Err(); err != nil {
		return AcknowledgementIndex{}, err
	}
	index := AcknowledgementIndex{ByID: map[ids.ExecutionID]ExecutionAcknowledgement{}}
	err := j.db.View(func(tx *bbolt.Tx) error {
		epoch, err := acknowledgementEpoch(tx)
		if err != nil {
			return err
		}
		index.Epoch = epoch
		bucket := tx.Bucket(bAcknowledgements)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(key, raw []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			value, err := decodeCollectionAcknowledgement(key, raw)
			if err != nil {
				return err
			}
			index.ByID[value.ExecutionID] = value
			return nil
		})
	})
	return index, err
}

func (j *Journal) GetAcknowledgement(ctx context.Context, id ids.ExecutionID) (ExecutionAcknowledgement, error) {
	if err := ctx.Err(); err != nil {
		return ExecutionAcknowledgement{}, err
	}
	var value ExecutionAcknowledgement
	err := j.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bAcknowledgements)
		if bucket == nil {
			return ErrNotFound
		}
		raw := bucket.Get([]byte(id.String()))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return corrupt(err)
		}
		return nil
	})
	return value, err
}

func (j *Journal) AcknowledgeExecution(ctx context.Context, id ids.ExecutionID, source string) (ExecutionAcknowledgement, bool, error) {
	if j.readOnly {
		return ExecutionAcknowledgement{}, false, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return ExecutionAcknowledgement{}, false, err
	}
	if source != AcknowledgementResult && source != AcknowledgementAwait && source != AcknowledgementBulk {
		return ExecutionAcknowledgement{}, false, fmt.Errorf("invalid acknowledgement source %q", source)
	}
	var result ExecutionAcknowledgement
	var reused bool
	err := j.update(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bExecutions).Get([]byte(id.String()))
		if raw == nil {
			return ErrNotFound
		}
		var execution model.Execution
		if err := json.Unmarshal(raw, &execution); err != nil {
			return corrupt(err)
		}
		if err := execution.Validate(); err != nil {
			return corrupt(err)
		}
		if !execution.State.Terminal() {
			return fmt.Errorf("%w: execution is not terminal", ErrConflict)
		}
		bucket, err := ensureJournalBucket(tx, bAcknowledgements)
		if err != nil {
			return err
		}
		if existing := bucket.Get([]byte(id.String())); existing != nil {
			if err := json.Unmarshal(existing, &result); err != nil {
				return corrupt(err)
			}
			reused = true
			return nil
		}
		result = ExecutionAcknowledgement{ExecutionID: id, AcknowledgedAt: j.clock().UTC(), Source: source}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(id.String()), encoded); err != nil {
			return err
		}
		if source == AcknowledgementBulk {
			if err := appendBulkAcknowledgementEvent(tx, j, execution, result.AcknowledgedAt); err != nil {
				return err
			}
		}
		return nil
	})
	return result, reused, err
}

func appendBulkAcknowledgementEvent(tx *bbolt.Tx, j *Journal, execution model.Execution, at time.Time) error {
	projection := map[string]any{
		"authority_scope":       string(execution.Authority),
		"execution_fingerprint": execution.ID.String(),
		"kind":                  string(model.EventAcknowledged),
		"source":                AcknowledgementBulk,
		"state":                 string(execution.State),
	}
	key, canonical, err := callback.SemanticDedupeKey(execution.Adapter, 1, projection)
	if err != nil {
		return err
	}
	state := execution.State
	event := model.Event{
		ExecutionID: execution.ID, OriginHostID: execution.OriginHostID, Ordering: model.OrderingObservation,
		Kind: model.EventAcknowledged, State: &state, Authority: execution.Authority, Adapter: execution.Adapter,
		ObservedAt: at, DedupeKey: key, DedupeVersion: 1,
		Payload: map[string]any{"acknowledgement_source": AcknowledgementBulk},
	}
	if err := validateEventProjection(event, canonical); err != nil {
		return err
	}
	_, _, err = j.appendEventTx(tx, event, canonical)
	return err
}

func acknowledgementEpoch(tx *bbolt.Tx) (time.Time, error) {
	metadata := tx.Bucket(bMetadata)
	if metadata == nil {
		return time.Time{}, nil
	}
	raw := metadata.Get(keyAckEpoch)
	if raw == nil {
		return time.Time{}, nil
	}
	value, err := time.Parse(time.RFC3339Nano, string(raw))
	if err != nil {
		value, err = time.Parse(time.RFC3339, string(raw))
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: acknowledgement epoch: %v", ErrCorrupt, err)
	}
	return value.UTC(), nil
}

// AcknowledgementIndexFor reads only the selected page and the epoch. Unlike
// the global index this does not walk retained acknowledgement history.
func (j *Journal) AcknowledgementIndexFor(ctx context.Context, executions []model.Execution) (AcknowledgementIndex, error) {
	index := AcknowledgementIndex{ByID: map[ids.ExecutionID]ExecutionAcknowledgement{}}
	err := j.db.View(func(tx *bbolt.Tx) error {
		var err error
		index.Epoch, err = acknowledgementEpoch(tx)
		if err != nil {
			return err
		}
		b := tx.Bucket(bAcknowledgements)
		if b == nil {
			return ctx.Err()
		}
		for _, execution := range executions {
			if err := ctx.Err(); err != nil {
				return err
			}
			raw := b.Get([]byte(execution.ID.String()))
			if raw == nil {
				continue
			}
			var ack ExecutionAcknowledgement
			if err := json.Unmarshal(raw, &ack); err != nil {
				return corrupt(err)
			}
			index.ByID[ack.ExecutionID] = ack
		}
		return nil
	})
	return index, err
}

// decodeCollectionAcknowledgement validates evidence used to retire child
// obligations. Keep the global inbox snapshot and selected child reads in
// agreement: a record's JSON identity cannot override its authoritative key.
func decodeCollectionAcknowledgement(key, raw []byte) (ExecutionAcknowledgement, error) {
	var value ExecutionAcknowledgement
	id, err := ids.ParseExecutionID(string(key))
	if err != nil {
		return value, corrupt(fmt.Errorf("invalid collection acknowledgement key: %w", err))
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, corrupt(err)
	}
	if value.ExecutionID != id || value.AcknowledgedAt.IsZero() || (value.Source != AcknowledgementResult && value.Source != AcknowledgementAwait && value.Source != AcknowledgementBulk) {
		return value, corrupt(fmt.Errorf("invalid collection acknowledgement identity, time or source"))
	}
	return value, nil
}
