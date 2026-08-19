package model

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

const SchemaVersion = 1

type Authority string

const (
	AuthorityNative  Authority = "native"
	AuthorityMultica Authority = "multica"
)

type Mode string

const (
	ModeDirect  Mode = "direct"
	ModeMultica Mode = "multica"
)

type Acquisition string

const (
	AcquisitionLaunched Acquisition = "launched"
	AcquisitionAttached Acquisition = "attached"
	AcquisitionPromoted Acquisition = "promoted"
)

type State string

const (
	StateCreated   State = "created"
	StateStarting  State = "starting"
	StateRunning   State = "running"
	StateWaiting   State = "waiting"
	StateAttention State = "attention"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StateOrphaned  State = "orphaned"
)

type Liveness string

const (
	LivenessUnknown     Liveness = "unknown"
	LivenessAlive       Liveness = "alive"
	LivenessBlocked     Liveness = "blocked"
	LivenessExited      Liveness = "exited"
	LivenessUnreachable Liveness = "unreachable"
)

func (s State) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled || s == StateOrphaned
}

type SourceBinding struct {
	Kind        string  `json:"kind"`
	AliasID     ids.ID  `json:"alias_id"`
	Fingerprint string  `json:"fingerprint"`
	OpaqueID    *string `json:"opaque_id,omitempty"`
}

type CapabilityStatus string

const (
	CapabilitySupported   CapabilityStatus = "supported"
	CapabilityDegraded    CapabilityStatus = "degraded"
	CapabilityUnavailable CapabilityStatus = "unavailable"
)

type CapabilityItem struct {
	Name             string           `json:"name"`
	Status           CapabilityStatus `json:"status"`
	Source           string           `json:"source"`
	SemanticsVersion int              `json:"semantics_version,omitempty"`
	Constraints      map[string]any   `json:"constraints,omitempty"`
	Reason           *string          `json:"reason,omitempty"`
}
type CapabilitySnapshot struct {
	NegotiatedAt   time.Time        `json:"negotiated_at"`
	AdapterVersion string           `json:"adapter_version"`
	BackendVersion *string          `json:"backend_version"`
	Items          []CapabilityItem `json:"items"`
}

type ObservationSource string

const (
	ObservationDurableOutbox ObservationSource = "durable_outbox"
	ObservationNativeStream  ObservationSource = "native_stream"
	ObservationStatusAPI     ObservationSource = "status_api"
	ObservationStateFile     ObservationSource = "state_file"
	ObservationPoll          ObservationSource = "poll"
	ObservationProcess       ObservationSource = "process"
	ObservationReconciled    ObservationSource = "reconciled"
	ObservationUnknown       ObservationSource = "unknown"
)

type Integrity string

const (
	IntegrityVerified   Integrity = "verified"
	IntegrityDegraded   Integrity = "degraded"
	IntegrityConflicted Integrity = "conflicted"
	IntegrityUnknown    Integrity = "unknown"
)

type Observation struct {
	Source          ObservationSource `json:"source"`
	Integrity       Integrity         `json:"integrity"`
	ObservedAt      time.Time         `json:"observed_at"`
	FreshForSeconds *int              `json:"fresh_for_seconds"`
}

type PromotionState string

const (
	PromotionPlanned          PromotionState = "planned"
	PromotionAuthorityCreated PromotionState = "authority_created"
	PromotionBindingsRecorded PromotionState = "bindings_recorded"
	PromotionActive           PromotionState = "active"
	PromotionRecoveryRequired PromotionState = "recovery_required"
)

type PromotionLink struct {
	Role                   string          `json:"role"`
	CounterpartExecutionID ids.ExecutionID `json:"counterpart_execution_id"`
	PromotionKey           string          `json:"promotion_key"`
	State                  PromotionState  `json:"state"`
}
type Continuation struct {
	SameSessionRequired bool `json:"same_session_required"`
}
type ExecutionProvenance struct {
	PortableSkillDigest string `json:"portable_skill_digest,omitempty"`
	ContextDigest       string `json:"context_digest,omitempty"`
	HandoffDigest       string `json:"handoff_digest,omitempty"`
}
type TaskContract struct {
	ObjectiveSummary      string               `json:"objective_summary,omitempty"`
	SideEffectBoundary    string               `json:"side_effect_boundary,omitempty"`
	AcceptanceRef         *ids.ContextID       `json:"acceptance_ref,omitempty"`
	ExpectedArtifactKinds []string             `json:"expected_artifact_kinds,omitempty"`
	Continuation          *Continuation        `json:"continuation,omitempty"`
	Provenance            *ExecutionProvenance `json:"provenance,omitempty"`
}

type Execution struct {
	SchemaVersion     int                `json:"schema_version"`
	ID                ids.ExecutionID    `json:"id"`
	OriginHostID      ids.HostID         `json:"origin_host_id"`
	Revision          uint64             `json:"revision"`
	Authority         Authority          `json:"authority"`
	Adapter           string             `json:"adapter"`
	Mode              Mode               `json:"mode"`
	Acquisition       Acquisition        `json:"acquisition"`
	State             State              `json:"state"`
	Liveness          Liveness           `json:"liveness"`
	SourceState       *string            `json:"source_state,omitempty"`
	SourceBindings    []SourceBinding    `json:"source_bindings"`
	Capabilities      CapabilitySnapshot `json:"capabilities"`
	Labels            []string           `json:"labels,omitempty"`
	CWD               *string            `json:"cwd,omitempty"`
	Repository        *string            `json:"repository,omitempty"`
	ParentExecutionID *ids.ExecutionID   `json:"parent_execution_id"`
	Supersedes        []ids.ExecutionID  `json:"supersedes"`
	SupersededBy      *ids.ExecutionID   `json:"superseded_by"`
	Promotion         *PromotionLink     `json:"promotion"`
	TaskContract      *TaskContract      `json:"task_contract,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	StartedAt         *time.Time         `json:"started_at,omitempty"`
	DeadlineAt        *time.Time         `json:"deadline_at,omitempty"`
	UpdatedAt         time.Time          `json:"updated_at"`
	TerminalAt        *time.Time         `json:"terminal_at"`
	Observation       Observation        `json:"observation"`
}

var (
	adapterPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	hashPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

func (e Execution) Validate() error {
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if _, err := ids.ParseExecutionID(e.ID.String()); err != nil {
		return fmt.Errorf("id: %w", err)
	}
	if _, err := ids.ParseHostID(e.OriginHostID.String()); err != nil {
		return fmt.Errorf("origin_host_id: %w", err)
	}
	if e.Revision < 1 {
		return errors.New("revision must be at least 1")
	}
	if !adapterPattern.MatchString(e.Adapter) {
		return errors.New("invalid adapter")
	}
	if (e.Mode == ModeDirect && e.Authority != AuthorityNative) || (e.Mode == ModeMultica && e.Authority != AuthorityMultica) {
		return errors.New("mode and authority disagree")
	}
	if e.Mode != ModeDirect && e.Mode != ModeMultica {
		return errors.New("invalid mode")
	}
	if e.Acquisition != AcquisitionLaunched && e.Acquisition != AcquisitionAttached && e.Acquisition != AcquisitionPromoted {
		return errors.New("invalid acquisition")
	}
	if !validState(e.State) || !validLiveness(e.Liveness) {
		return errors.New("invalid state or liveness")
	}
	if e.State.Terminal() != (e.TerminalAt != nil) {
		return errors.New("terminal_at must be set exactly for terminal state")
	}
	if e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() || e.Observation.ObservedAt.IsZero() {
		return errors.New("required timestamp is zero")
	}
	if e.DeadlineAt != nil && e.DeadlineAt.IsZero() {
		return errors.New("deadline_at is zero")
	}
	if e.UpdatedAt.Before(e.CreatedAt) {
		return errors.New("updated_at precedes created_at")
	}
	if e.Observation.FreshForSeconds != nil && *e.Observation.FreshForSeconds < 0 {
		return errors.New("fresh_for_seconds is negative")
	}
	if !validObservation(e.Observation.Source) || !validIntegrity(e.Observation.Integrity) {
		return errors.New("invalid observation")
	}
	for i, b := range e.SourceBindings {
		if err := validateBinding(b); err != nil {
			return fmt.Errorf("source_bindings[%d]: %w", i, err)
		}
	}
	if e.Capabilities.AdapterVersion == "" || e.Capabilities.NegotiatedAt.IsZero() {
		return errors.New("invalid capability snapshot")
	}
	if len(e.Labels) > 16 {
		return errors.New("too many labels")
	}
	seenLabels := map[string]struct{}{}
	for _, label := range e.Labels {
		if !namePattern.MatchString(label) {
			return errors.New("invalid label")
		}
		if _, exists := seenLabels[label]; exists {
			return errors.New("duplicate label")
		}
		seenLabels[label] = struct{}{}
	}
	for _, item := range e.Capabilities.Items {
		if !namePattern.MatchString(item.Name) || !namePattern.MatchString(item.Source) {
			return errors.New("invalid capability item")
		}
	}
	if e.Promotion != nil {
		if e.Promotion.Role != "source" && e.Promotion.Role != "target" {
			return errors.New("invalid promotion role")
		}
		if _, err := ids.ParseExecutionID(e.Promotion.CounterpartExecutionID.String()); err != nil {
			return fmt.Errorf("promotion counterpart: %w", err)
		}
		if !hashPattern.MatchString(e.Promotion.PromotionKey) {
			return errors.New("invalid promotion key")
		}
		switch e.Promotion.State {
		case PromotionPlanned, PromotionAuthorityCreated, PromotionBindingsRecorded, PromotionActive, PromotionRecoveryRequired:
		default:
			return errors.New("invalid promotion state")
		}
	}
	if e.TaskContract != nil && len(e.TaskContract.ObjectiveSummary) > 2048 {
		return errors.New("objective summary exceeds 2048 bytes")
	}
	if e.TaskContract != nil && e.TaskContract.Provenance != nil {
		for name, digest := range map[string]string{
			"portable_skill_digest": e.TaskContract.Provenance.PortableSkillDigest,
			"context_digest":        e.TaskContract.Provenance.ContextDigest,
			"handoff_digest":        e.TaskContract.Provenance.HandoffDigest,
		} {
			if digest != "" && !hashPattern.MatchString(digest) {
				return fmt.Errorf("invalid task provenance %s", name)
			}
		}
	}
	return nil
}

func ValidateTransition(previous, next Execution) error {
	if previous.ID != next.ID || previous.OriginHostID != next.OriginHostID {
		return errors.New("execution identity is immutable")
	}
	if previous.Authority != next.Authority || previous.Mode != next.Mode || previous.Acquisition != next.Acquisition {
		return errors.New("execution authority and acquisition are immutable")
	}
	if !slices.Equal(previous.Labels, next.Labels) {
		return errors.New("execution labels are immutable")
	}
	if previous.CreatedAt != next.CreatedAt {
		return errors.New("created_at is immutable")
	}
	if previous.State.Terminal() && previous.State != next.State {
		return errors.New("terminal execution state is immutable")
	}
	if previous.SupersededBy != nil && (next.SupersededBy == nil || *previous.SupersededBy != *next.SupersededBy) {
		return errors.New("superseded_by is assigned once")
	}
	return next.Validate()
}

func validState(v State) bool {
	switch v {
	case StateCreated, StateStarting, StateRunning, StateWaiting, StateAttention, StateCompleted, StateFailed, StateCancelled, StateOrphaned:
		return true
	}
	return false
}
func validLiveness(v Liveness) bool {
	switch v {
	case LivenessUnknown, LivenessAlive, LivenessBlocked, LivenessExited, LivenessUnreachable:
		return true
	}
	return false
}
func validObservation(v ObservationSource) bool {
	switch v {
	case ObservationDurableOutbox, ObservationNativeStream, ObservationStatusAPI, ObservationStateFile, ObservationPoll, ObservationProcess, ObservationReconciled, ObservationUnknown:
		return true
	}
	return false
}
func validIntegrity(v Integrity) bool {
	switch v {
	case IntegrityVerified, IntegrityDegraded, IntegrityConflicted, IntegrityUnknown:
		return true
	}
	return false
}
func validateBinding(v SourceBinding) error {
	if !namePattern.MatchString(v.Kind) {
		return errors.New("invalid kind")
	}
	if v.AliasID.IsZero() {
		return errors.New("alias_id is required")
	}
	switch v.AliasID.Type() {
	case ids.TypeSource, ids.TypeProject, ids.TypeIssue, ids.TypeRun:
	default:
		return errors.New("invalid alias_id type")
	}
	if !hashPattern.MatchString(v.Fingerprint) {
		return errors.New("invalid fingerprint")
	}
	return nil
}
