package anxrun

import (
	"fmt"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
)

// RegistrationSchemaVersion versions the spooled registration document. The
// ANX-side bridge can consume the same shape when the core run fields land.
const RegistrationSchemaVersion = 5

// Phase distinguishes the two registration points for one execution.
type Phase string

const (
	// PhaseStart registers a running execution. It is spooled only; launch
	// never waits on ANX.
	PhaseStart Phase = "start"
	// PhaseTerminal registers the terminal outcome. Delivery refreshes, never
	// regresses, the registered state.
	PhaseTerminal Phase = "terminal"
)

// ANX state and liveness enums from the public runs surface. agentctl state
// maps deterministically; values the API does not accept today (outcome,
// native_session, transcript, supersedes) ride in the registration document
// for the ANX-side extension.
const (
	StateStarting  = "starting"
	StateRunning   = "running"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateUnknown   = "unknown"

	LivenessAlive  = "alive"
	LivenessStale  = "stale"
	LivenessUnknow = "unknown"
)

// OutcomeAvailability values for the registration outcome block.
const (
	AvailabilityStored              = "stored"
	AvailabilityOmittedByPolicy     = "omitted_by_policy"
	AvailabilityUnavailableAtSource = "unavailable_at_source"
)

// Registration is the durable run-link document spooled for one execution
// phase. Key identity is the durable ID (origin_host_id + external_id) plus
// phase; re-enqueueing the same phase is an idempotent overwrite.
type Registration struct {
	SchemaVersion int    `json:"schema_version"`
	Phase         Phase  `json:"phase"`
	Launcher      string `json:"launcher"`
	ExternalID    string `json:"external_id"`
	OriginHostID  string `json:"origin_host_id"`
	DurableID     string `json:"durable_id"`
	// RequestKey is transient local input, excluded from every JSON encoding.
	// Enqueue replaces it with a host-keyed HMAC before persistence.
	RequestKey     string      `json:"-"`
	RequestKeyHash string      `json:"request_key_hash,omitempty"`
	Adapter        string      `json:"adapter"`
	Model          string      `json:"model,omitempty"`
	State          string      `json:"state"`
	Liveness       string      `json:"liveness"`
	CWD            string      `json:"cwd,omitempty"`
	Repository     *Repository `json:"repository,omitempty"`
	Labels         []string    `json:"-"`
	CardRefs       []string    `json:"card_refs,omitempty"`
	StartedAt      *time.Time  `json:"started_at,omitempty"`
	EndedAt        *time.Time  `json:"ended_at,omitempty"`
	// ResultCollected mirrors the journal acknowledgement state at build time.
	ResultCollected bool `json:"result_collected"`
	// Sequence is the monotonic sessions/participation sequence assigned at
	// enqueue time; replaying a spooled entry reuses it.
	Sequence             int            `json:"sequence"`
	Outcome              *Outcome       `json:"outcome,omitempty"`
	NativeSession        *NativeSession `json:"native_session,omitempty"`
	SupersedesExternalID string         `json:"supersedes_external_id,omitempty"`
	EgressErrorCode      int            `json:"egress_error_code,omitempty"`
	BuiltAt              time.Time      `json:"built_at"`
}

// NativeSession carries the identity v1 correlator for the native transcript.
// The raw native session id never leaves the origin host and the backup.
type NativeSession struct {
	Provider    string `json:"provider"`
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint"`
}

// Outcome contains only typed metadata and bounded structured evidence.
type Outcome struct {
	Availability    string             `json:"availability"`
	Failure         *FailureProjection `json:"failure,omitempty"`
	Links           []Link             `json:"links,omitempty"`
	EgressErrorCode int                `json:"-"`
	// HeadAfter is the bare 40-hex git HEAD in the launch cwd at terminal, or
	// empty when it could not be read.
	HeadAfter string `json:"head_after,omitempty"`
}

// FailureProjection is the bounded failure projection from the stored outcome.
type FailureProjection struct {
	Code      string `json:"code"`
	Kind      string `json:"kind,omitempty"`
	Retryable bool   `json:"retryable"`
}

// ANXState maps an agentctl execution state onto the ANX run state enum.
// Orphaned means the outcome is unknown, not failed.
func ANXState(state model.State) string {
	switch state {
	case model.StateCreated, model.StateStarting:
		return StateStarting
	case model.StateRunning, model.StateWaiting, model.StateAttention:
		return StateRunning
	case model.StateCompleted:
		return StateCompleted
	case model.StateFailed:
		return StateFailed
	case model.StateCancelled:
		return StateCancelled
	default:
		return StateUnknown
	}
}

// ANXLiveness maps an agentctl liveness onto the ANX enum. ANX has no exited
// value; a terminal (exited) run reports stale, the honest not-alive value.
func ANXLiveness(liveness model.Liveness) string {
	switch liveness {
	case model.LivenessAlive, model.LivenessBlocked:
		return LivenessAlive
	case model.LivenessExited, model.LivenessUnreachable:
		return LivenessStale
	default:
		return LivenessUnknow
	}
}

// SessionActivity maps run state onto the sessions/participation activity
// enum: running -> active, terminal -> idle.
func SessionActivity(state model.State) string {
	if state.Terminal() {
		return "idle"
	}
	return "active"
}

// BuildStart builds the PhaseStart registration for one execution.
func BuildStart(execution model.Execution, now time.Time) Registration {
	registration := Registration{
		SchemaVersion: RegistrationSchemaVersion,
		Phase:         PhaseStart,
		Launcher:      "agentctl",
		ExternalID:    execution.ID.String(),
		OriginHostID:  execution.OriginHostID.String(),
		DurableID:     fmt.Sprintf("agentctl://%s/%s", execution.OriginHostID, execution.ID),
		RequestKey:    delegationRequestKey(execution),
		Adapter:       execution.Adapter,
		Model:         "other",
		State:         ANXState(execution.State),
		Liveness:      ANXLiveness(execution.Liveness),

		StartedAt:     execution.StartedAt,
		NativeSession: nativeSession(execution),
		BuiltAt:       now.UTC(),
	}
	registration.CardRefs, registration.EgressErrorCode = resolveCards(execution.Labels)
	if execution.CWD != nil {
		registration.CWD = *execution.CWD
	}
	return registration
}

// BuildTerminal builds the PhaseTerminal registration for one execution and
// its stored outcome. The outcome is read through the journal's read-only
// accessor. Only allowlisted metadata and structured evidence leave the host.
func BuildTerminal(execution model.Execution, outcome *model.Outcome, resultCollected bool, now time.Time) Registration {
	registration := BuildStart(execution, now)
	registration.Phase = PhaseTerminal
	registration.State = ANXState(execution.State)
	registration.Liveness = ANXLiveness(execution.Liveness)
	registration.EndedAt = execution.TerminalAt
	registration.ResultCollected = resultCollected
	for _, superseded := range execution.Supersedes {
		registration.SupersedesExternalID = superseded.String()
	}
	if outcome != nil {
		registration.Outcome = ProjectOutcome(*outcome)
	} else {
		registration.Outcome = &Outcome{Availability: AvailabilityUnavailableAtSource}
	}
	return registration
}

// ProjectOutcome projects only enum metadata and explicit structured evidence.
// Free-text results, including formerly marked summaries, are never exported.
func ProjectOutcome(outcome model.Outcome) *Outcome {
	projected := &Outcome{Availability: safeAvailability(string(outcome.Availability))}
	if outcome.Failure != nil {
		projected.Failure = &FailureProjection{Code: "execution_failed", Retryable: outcome.Failure.Retryable}
	}
	for _, e := range outcome.Evidence {
		projected.Links = append(projected.Links, Link{Kind: e.Kind, Owner: e.Owner, Repo: e.Repo, Number: e.Number, SHA: e.SHA})
		if len(projected.Links) == MaxLinks {
			break
		}
	}
	return projected
}

// nativeSessionBindingKind is the journal source-binding kind that carries
// the raw native session id (cmd nativeSessionBinding in package main).
const nativeSessionBindingKind = "native_session"

// nativeSession derives the identity v1 correlator from the journal's private
// native session binding. Multica-authority executions have no native session.
func nativeSession(execution model.Execution) *NativeSession {
	if execution.Authority != model.AuthorityNative {
		return nil
	}
	for _, binding := range execution.SourceBindings {
		if binding.Kind != nativeSessionBindingKind || binding.OpaqueID == nil {
			continue
		}
		provider := CanonicalProvider(execution.Adapter)
		sessionID := *binding.OpaqueID
		if provider == "" || sessionID == "" {
			return nil
		}
		return &NativeSession{Provider: provider, Kind: NativeSessionIDKind, Fingerprint: NativeSessionFingerprint(provider, sessionID)}
	}
	return nil
}

// delegationRequestKey recovers the raw delegate request key from the frozen
// delegation binding. Expert runs without a delegation carry none; legacy
// delegated records predating the field also read empty.
func delegationRequestKey(execution model.Execution) string {
	if execution.Delegation == nil {
		return ""
	}
	return execution.Delegation.RequestKey
}

// delegationModel reports the resolved model the delegation actually ran.
func delegationModel(execution model.Execution) string {
	if execution.Delegation == nil {
		return ""
	}
	return execution.Delegation.Resolved.Model
}
