package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

type Ordering string

const (
	OrderingSource      Ordering = "source"
	OrderingObservation Ordering = "observation"
	OrderingReconciled  Ordering = "reconciled"
)

type EventKind string

const (
	EventStarted    EventKind = "started"
	EventProgress   EventKind = "progress"
	EventAttention  EventKind = "attention"
	EventArtifact   EventKind = "artifact"
	EventHealth     EventKind = "health"
	EventTerminal   EventKind = "terminal"
	EventPromoted   EventKind = "promoted"
	EventSuperseded EventKind = "superseded"
)

type SourcePosition struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type Event struct {
	SchemaVersion  int             `json:"schema_version"`
	ID             ids.EventID     `json:"id"`
	OriginHostID   ids.HostID      `json:"origin_host_id"`
	ExecutionID    ids.ExecutionID `json:"execution_id"`
	Sequence       uint64          `json:"sequence"`
	Ordering       Ordering        `json:"ordering"`
	SourcePosition *SourcePosition `json:"source_position,omitempty"`
	Kind           EventKind       `json:"kind"`
	State          *State          `json:"state,omitempty"`
	SourceState    *string         `json:"source_state,omitempty"`
	Authority      Authority       `json:"authority"`
	Adapter        string          `json:"adapter"`
	OccurredAt     *time.Time      `json:"occurred_at"`
	ObservedAt     time.Time       `json:"observed_at"`
	DedupeKey      string          `json:"dedupe_key"`
	DedupeVersion  int             `json:"dedupe_version"`
	Payload        map[string]any  `json:"payload"`
}

func (e Event) Validate() error {
	if e.SchemaVersion != SchemaVersion {
		return errors.New("unsupported event schema_version")
	}
	if _, err := ids.ParseEventID(e.ID.String()); err != nil {
		return fmt.Errorf("id: %w", err)
	}
	if _, err := ids.ParseHostID(e.OriginHostID.String()); err != nil {
		return fmt.Errorf("origin_host_id: %w", err)
	}
	if _, err := ids.ParseExecutionID(e.ExecutionID.String()); err != nil {
		return fmt.Errorf("execution_id: %w", err)
	}
	if e.Sequence < 1 {
		return errors.New("sequence must be at least 1")
	}
	if e.Ordering != OrderingSource && e.Ordering != OrderingObservation && e.Ordering != OrderingReconciled {
		return errors.New("invalid ordering")
	}
	if (e.Ordering == OrderingSource || e.Ordering == OrderingReconciled) && e.SourcePosition == nil {
		return errors.New("source_position required for source/reconciled ordering")
	}
	if e.SourcePosition != nil && (!namePattern.MatchString(e.SourcePosition.Kind) || e.SourcePosition.Value == "" || len(e.SourcePosition.Value) > 512) {
		return errors.New("invalid source_position")
	}
	if !validEventKind(e.Kind) {
		return errors.New("invalid event kind")
	}
	if e.Kind == EventTerminal && (e.State == nil || !e.State.Terminal()) {
		return errors.New("terminal event requires terminal state")
	}
	if !adapterPattern.MatchString(e.Adapter) {
		return errors.New("invalid adapter")
	}
	if e.Authority != AuthorityNative && e.Authority != AuthorityMultica {
		return errors.New("invalid authority")
	}
	if e.ObservedAt.IsZero() {
		return errors.New("observed_at is required")
	}
	if !hashPattern.MatchString(e.DedupeKey) || e.DedupeVersion < 1 {
		return errors.New("invalid dedupe contract")
	}
	if e.Payload == nil {
		return errors.New("payload is required")
	}
	return ValidateBoundedMetadata(e.Payload, 64*1024)
}

func validEventKind(v EventKind) bool {
	switch v {
	case EventStarted, EventProgress, EventAttention, EventArtifact, EventHealth, EventTerminal, EventPromoted, EventSuperseded:
		return true
	}
	return false
}
func badPayloadKey(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			k := strings.ToLower(key)
			switch k {
			case "prompt", "raw_prompt", "transcript", "reasoning", "chain_of_thought", "stdout", "stderr", "raw_output", "token_delta", "tokens", "opaque_id", "session_id", "thread_id", "run_id", "issue_id", "workspace_id", "project_id":
				return true
			}
			if badPayloadKey(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if badPayloadKey(item) {
				return true
			}
		}
	}
	return false
}

// ValidateBoundedMetadata enforces the journal privacy boundary for adapter
// projections and event payloads. It rejects the common raw-content fields;
// callers should persist references or bounded summaries instead.
func ValidateBoundedMetadata(value any, maxBytes int) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if maxBytes > 0 && len(encoded) > maxBytes {
		return fmt.Errorf("metadata exceeds %d bytes", maxBytes)
	}
	if badPayloadKey(value) {
		return errors.New("metadata contains raw prompt, transcript, reasoning, or output field")
	}
	return nil
}
