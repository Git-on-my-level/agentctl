// Package adapter contains the small, transport-neutral boundary between
// agentctl and native agent runtimes.  Adapters deliberately deal in argv and
// structured observations; they never invoke a shell or persist transcripts.
package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	ManifestSchemaVersion = 1
	SemanticsVersion      = 1
	AdapterAPIVersion     = "1.0.0"
)

// CapabilityName is one of the versioned adapter capabilities from the
// adapter contract. Unknown names are intentionally representable so readers
// can preserve forward-compatible manifests.
type CapabilityName string

const (
	CapabilityLaunch             CapabilityName = "launch"
	CapabilityAttach             CapabilityName = "attach"
	CapabilitySnapshot           CapabilityName = "snapshot"
	CapabilityEvents             CapabilityName = "events"
	CapabilityResult             CapabilityName = "result"
	CapabilityResultContent      CapabilityName = "result_content"
	CapabilityResume             CapabilityName = "resume"
	CapabilityCancel             CapabilityName = "cancel"
	CapabilityArtifacts          CapabilityName = "artifacts"
	CapabilityHistory            CapabilityName = "history"
	CapabilityContextInjection   CapabilityName = "context_injection"
	CapabilityPromotion          CapabilityName = "promotion"
	CapabilityDurableIdempotency CapabilityName = "durable_idempotency"
	CapabilityDurableEvents      CapabilityName = "durable_events"
	CapabilityRemoteCallback     CapabilityName = "remote_callback"
)

type CapabilityStatus string

const (
	CapabilitySupported   CapabilityStatus = "supported"
	CapabilityDegraded    CapabilityStatus = "degraded"
	CapabilityUnavailable CapabilityStatus = "unavailable"
	// CapabilityConditional is used only by static manifest declarations. A
	// probe resolves it to supported or degraded for the instance snapshot.
	CapabilityConditional CapabilityStatus = "conditional"
)

// Capability is both a manifest declaration and a probed capability item.
// Source identifies the evidence used to make the claim (manifest, native
// stream, bounded_poll, and so on).
type Capability struct {
	Name             CapabilityName   `json:"name"`
	Status           CapabilityStatus `json:"status"`
	Source           string           `json:"source"`
	SemanticsVersion int              `json:"semantics_version"`
	Constraints      map[string]any   `json:"constraints,omitempty"`
	Reason           string           `json:"reason,omitempty"`
}

// CapabilityDeclaration is the static part of a manifest.
type CapabilityDeclaration struct {
	Name             CapabilityName   `json:"name"`
	SemanticsVersion int              `json:"semantics_version"`
	Implementation   CapabilityStatus `json:"implementation"`
	Constraints      map[string]any   `json:"constraints"`
}

type KnownFailure struct {
	Classifier string `json:"classifier"`
	ErrorCode  string `json:"error_code"`
}

type ExecutableRule struct {
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Required bool   `json:"required,omitempty"`
}

type BackendCompatibility struct {
	Range  string `json:"range"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type ProbeAction struct {
	ID              string   `json:"id"`
	Argv            []string `json:"argv"`
	SideEffectClass string   `json:"side_effect_class"`
	WritesCache     bool     `json:"writes_cache"`
	TimeoutSeconds  int      `json:"timeout_seconds,omitempty"`
}

type ContextMechanism string

const (
	ContextEnvironmentPath   ContextMechanism = "environment_path"
	ContextNativeArgument    ContextMechanism = "native_argument"
	ContextInstructionFile   ContextMechanism = "native_instruction_file"
	ContextAuthorityArtifact ContextMechanism = "authority_artifact_ref"
)

// ContextInjection declares how context can be handed to a child. Guaranteed
// means the adapter itself has a native delivery guarantee; an environment
// variable alone is never treated as model-readable context.
type ContextInjection struct {
	Mechanisms []ContextMechanism `json:"mechanisms,omitempty"`
	Guaranteed bool               `json:"guaranteed"`
	Reason     string             `json:"reason,omitempty"`
}

type StructuredOutputSpec struct {
	Format        string   `json:"format"`
	Required      bool     `json:"required"`
	DiscoveryKeys []string `json:"discovery_keys,omitempty"`
	TerminalKeys  []string `json:"terminal_keys,omitempty"`
}

// Manifest is machine-readable metadata for an adapter implementation.
type Manifest struct {
	SchemaVersion          int                     `json:"schema_version"`
	Adapter                string                  `json:"adapter"`
	AdapterVersion         string                  `json:"adapter_version"`
	CoreCompatibility      VersionRange            `json:"core_compatibility"`
	DocumentSchemaVersions map[string][]int        `json:"document_schema_versions,omitempty"`
	ExecutableDiscovery    []ExecutableRule        `json:"executable_discovery"`
	BackendCompatibility   []BackendCompatibility  `json:"backend_compatibility"`
	Capabilities           []CapabilityDeclaration `json:"capabilities"`
	ProbeActions           []ProbeAction           `json:"probe_actions"`
	StructuredOutput       StructuredOutputSpec    `json:"structured_output,omitempty"`
	ContextInjection       ContextInjection        `json:"context_injection,omitempty"`
	KnownFailures          []KnownFailure          `json:"known_failures,omitempty"`
	Sensitivity            map[string]string       `json:"sensitivity,omitempty"`
}

type VersionRange struct {
	Minimum          string `json:"minimum"`
	MaximumExclusive string `json:"maximum_exclusive"`
}

// ProbeRequest identifies the exact executable/backend scope being checked.
// Endpoint/profile/workspace values participate in the returned fingerprint.
type ProbeRequest struct {
	Executable string
	Argv       []string
	Profile    string
	Endpoint   string
	Workspace  string
	Timeout    time.Duration
	Fresh      bool
}

type ProbeResult struct {
	AdapterVersion   string        `json:"adapter_version"`
	BackendVersion   string        `json:"backend_version,omitempty"`
	Executable       string        `json:"executable,omitempty"`
	ExecutableDigest string        `json:"executable_digest,omitempty"`
	ScopeFingerprint string        `json:"scope_fingerprint,omitempty"`
	ProbedAt         time.Time     `json:"probed_at"`
	FreshFor         time.Duration `json:"fresh_for"`
	ReadOnly         bool          `json:"read_only"`
	Capabilities     []Capability  `json:"capabilities"`
	Warnings         []string      `json:"warnings,omitempty"`
}

type PollOptions struct {
	Interval    time.Duration
	MaxInterval time.Duration
	Attempts    int
	Fresh       bool
}

func (p PollOptions) normalized() PollOptions {
	if p.Interval <= 0 {
		p.Interval = 15 * time.Second
	}
	if p.MaxInterval <= 0 {
		p.MaxInterval = p.Interval
	}
	if p.MaxInterval < p.Interval {
		p.MaxInterval = p.Interval
	}
	if p.Attempts <= 0 {
		p.Attempts = 1
	}
	return p
}

type SourceRef struct {
	Adapter     string `json:"adapter"`
	Kind        string `json:"kind"`
	OpaqueID    string `json:"opaque_id,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Profile     string `json:"profile,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	Issue       string `json:"issue,omitempty"`
	Run         string `json:"run,omitempty"`
	PID         int    `json:"pid,omitempty"`
}

func (r SourceRef) Empty() bool { return r.Kind == "" && r.OpaqueID == "" && r.Fingerprint == "" }

type SourceBinding struct {
	Kind        string `json:"kind"`
	AliasID     string `json:"alias_id,omitempty"`
	Fingerprint string `json:"fingerprint"`
	OpaqueID    string `json:"opaque_id,omitempty"`
}

func (r SourceRef) Binding() SourceBinding {
	id := r.OpaqueID
	if id == "" {
		id = strings.Join([]string{r.Profile, r.Endpoint, r.Workspace, r.Issue, r.Run}, "\x00")
	}
	fp := r.Fingerprint
	if fp == "" {
		fp = Fingerprint(r.Adapter, r.Kind, id)
	}
	return SourceBinding{Kind: r.Kind, Fingerprint: fp, OpaqueID: r.OpaqueID}
}

// Fingerprint binds exact source identity. It is deliberately not a display
// alias and is stable across process restarts.
func Fingerprint(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

type ContextInput struct {
	Path        string
	ArtifactRef string
	Instruction string
	Required    bool
}

type LaunchRequest struct {
	Argv            []string
	Stdin           []byte
	Cwd             string
	Env             []string
	Context         *ContextInput
	Timeout         time.Duration
	DiscoveryWindow time.Duration
	ResultPath      string
	// StartOnly keeps the process under this adapter's same-process
	// supervision and returns after discovery. The default is false: a
	// daemonless caller owns the child only while it remains alive, so normal
	// launches wait and reap the child before returning.
	StartOnly bool
}

type AttachRequest struct {
	Ref      SourceRef
	Poll     PollOptions
	ReadOnly bool
}

type SnapshotRequest struct {
	Ref  SourceRef
	Poll PollOptions
}

type EventsRequest struct {
	Ref    SourceRef
	Cursor string
	Poll   PollOptions
}

// MulticaEventPage is one bounded workspace event-list response. NextCursor
// is returned even when all events were filtered so a caller can advance its
// workspace cursor without ingesting unrelated authority events.
type MulticaEventPage struct {
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor,omitempty"`
	Scanned    int     `json:"scanned"`
	Filtered   int     `json:"filtered"`
	PageLimit  int     `json:"page_limit"`
}

// PagedEvents is implemented by the Multica adapter. The generic Events
// method returns only matching events; callers that need cursor advancement
// across filtered pages should use this bounded page projection.
type PagedEvents interface {
	EventsPage(context.Context, EventsRequest) (MulticaEventPage, error)
}

type ResultRequest struct {
	Ref  SourceRef
	Poll PollOptions
}

type ResumeRequest struct {
	Ref             SourceRef
	Argv            []string
	Context         *ContextInput
	Timeout         time.Duration
	DiscoveryWindow time.Duration
}

type CancelRequest struct {
	Ref    SourceRef
	Signal string
	Grace  time.Duration
}

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

type Observation struct {
	Source     string        `json:"source"`
	Integrity  string        `json:"integrity"`
	ObservedAt time.Time     `json:"observed_at"`
	FreshFor   time.Duration `json:"fresh_for"`
}

type Session struct {
	Ref            SourceRef     `json:"ref"`
	Binding        SourceBinding `json:"binding"`
	State          State         `json:"state"`
	Liveness       Liveness      `json:"liveness"`
	BackendVersion string        `json:"backend_version,omitempty"`
	ExitCode       *int          `json:"exit_code,omitempty"`
	StartedAt      time.Time     `json:"started_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	Observation    Observation   `json:"observation"`
}

type LaunchResult struct {
	Session Session `json:"session"`
	Result  *Result `json:"result,omitempty"`
}

type Attachment struct {
	Session Session `json:"session"`
}

type Snapshot struct {
	Session Session `json:"session"`
}

type Event struct {
	Sequence       int64          `json:"sequence"`
	Cursor         string         `json:"cursor,omitempty"`
	SourcePosition string         `json:"source_position,omitempty"`
	Kind           string         `json:"kind"`
	State          State          `json:"state,omitempty"`
	SourceState    string         `json:"source_state,omitempty"`
	OccurredAt     *time.Time     `json:"occurred_at,omitempty"`
	ObservedAt     time.Time      `json:"observed_at"`
	Ordering       string         `json:"ordering"`
	DedupeKey      string         `json:"dedupe_key"`
	Payload        map[string]any `json:"payload,omitempty"`
}

type Result struct {
	Success          bool           `json:"success"`
	State            State          `json:"state"`
	Summary          string         `json:"summary,omitempty"`
	Content          string         `json:"content,omitempty"`
	ContentType      string         `json:"content_type,omitempty"`
	ContentTruncated bool           `json:"content_truncated,omitempty"`
	Error            string         `json:"error,omitempty"`
	ExitCode         *int           `json:"exit_code,omitempty"`
	SessionRef       SourceRef      `json:"session_ref"`
	Data             map[string]any `json:"data,omitempty"`
}

type ErrorCode string

const (
	ErrCapabilityUnavailable  ErrorCode = "capability_unavailable"
	ErrDependencyUnavailable  ErrorCode = "dependency_unavailable"
	ErrAuthenticationRequired ErrorCode = "authentication_required"
	ErrNotFound               ErrorCode = "not_found"
	ErrTimeout                ErrorCode = "timeout"
	ErrExecutionFailed        ErrorCode = "execution_failed"
	ErrExecutionCancelled     ErrorCode = "execution_cancelled"
	ErrExecutionUnknown       ErrorCode = "execution_unknown"
	ErrUsage                  ErrorCode = "usage"
	ErrInternal               ErrorCode = "internal"
)

type AdapterError struct {
	Code      ErrorCode      `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
	Cause     error          `json:"-"`
}

func (e *AdapterError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return e.Message
}
func (e *AdapterError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func capabilityError(name CapabilityName, reason string) error {
	if reason == "" {
		reason = fmt.Sprintf("adapter does not support %s", name)
	}
	return &AdapterError{Code: ErrCapabilityUnavailable, Message: reason, Details: map[string]any{"capability": string(name)}}
}

func dependencyError(message string, cause error) error {
	return &AdapterError{Code: ErrDependencyUnavailable, Message: message, Retryable: true, Cause: cause}
}

func invalidRequest(message string) error {
	return &AdapterError{Code: ErrUsage, Message: message}
}

// Adapter is deliberately independent from the execution journal. The core
// can convert these records into its model without giving an adapter access to
// persistence, credentials, or raw session history.
type Adapter interface {
	Name() string
	Manifest() Manifest
	Probe(context.Context, ProbeRequest) (ProbeResult, error)
	Launch(context.Context, LaunchRequest) (LaunchResult, error)
	Wait(context.Context, SourceRef) (Result, error)
	Attach(context.Context, AttachRequest) (Attachment, error)
	Snapshot(context.Context, SnapshotRequest) (Snapshot, error)
	Events(context.Context, EventsRequest) ([]Event, error)
	Result(context.Context, ResultRequest) (Result, error)
	Resume(context.Context, ResumeRequest) (LaunchResult, error)
	Cancel(context.Context, CancelRequest) error
}

func marshalStable(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

var _ error = (*AdapterError)(nil)
