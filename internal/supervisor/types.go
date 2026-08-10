// Package supervisor provides the optional managed, host-local supervisor.
//
// The supervisor deliberately does not own execution or callback authority.
// Callers provide small repository/reprobe and outbox/delivery adapters.  This
// keeps the managed process useful with both native and Multica-backed
// executions while preserving the authority boundaries in the design docs.
package supervisor

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var ErrAttemptStartUnsupported = errors.New("durable outbox does not support attempt preparation")

var (
	// ErrAlreadyRunning is returned when Start is called for a running service.
	ErrAlreadyRunning = errors.New("supervisor already running")
	// ErrNotRunning is returned by Shutdown when no service is running.
	ErrNotRunning = errors.New("supervisor is not running")
	// ErrSocketExists means the configured socket path is not a stale Unix
	// socket that the supervisor can safely replace.
	ErrSocketExists = errors.New("supervisor socket path already exists")
	// ErrInsecurePermissions means a state directory, state file, or socket has
	// group/world permissions.  Sensitive operations fail closed in this case.
	ErrInsecurePermissions = errors.New("supervisor path has insecure permissions")
	// ErrSymlinkPath means a mutable supervisor path contains a symlink
	// component.  Paths are rejected before any mkdir/listen/remove operation.
	ErrSymlinkPath = errors.New("supervisor path contains a symlink")
	// ErrTailnetHookDisabled is returned unless a caller explicitly opts into
	// the authenticated Tailnet HTTP interface.
	ErrTailnetHookDisabled = errors.New("Tailnet HTTP hook is disabled")
	// ErrTailnetAuthenticatorRequired prevents accidentally exposing an
	// unauthenticated network handler.
	ErrTailnetAuthenticatorRequired = errors.New("Tailnet HTTP hook requires an authenticator")
)

// Clock is injectable so recovery and delivery tests do not depend on wall
// clock timing.  Production uses RealClock.
type Clock interface {
	Now() time.Time
}

// RealClock is the production clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// Execution is the minimum normalized execution data needed by the
// supervisor.  The core package may adapt its richer execution envelope to
// this value without giving the supervisor ownership of that envelope.
type Execution struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Liveness  string    `json:"liveness,omitempty"`
	Revision  uint64    `json:"revision,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// IsTerminal reports the terminal execution states from the execution
// envelope contract.
func (e Execution) IsTerminal() bool {
	switch e.State {
	case "completed", "failed", "cancelled", "orphaned":
		return true
	default:
		return false
	}
}

// ProbeResult is authoritative evidence obtained after a restart.  State is
// intentionally a string to allow adapters to preserve newer states without
// requiring a supervisor release.
type ProbeResult struct {
	State      string    `json:"state"`
	Liveness   string    `json:"liveness,omitempty"`
	Revision   uint64    `json:"revision,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source,omitempty"`
}

// ExecutionRepository is the host-local journal adapter used for recovery.
// ListNonTerminal must return only executions whose state is not terminal;
// the supervisor sorts the result by ID for deterministic behavior.
type ExecutionRepository interface {
	ListNonTerminal(context.Context) ([]Execution, error)
	ApplyProbe(context.Context, string, ProbeResult) error
}

// Reprober asks the authority adapter for fresh evidence about one execution.
// A temporary/unavailable answer should be represented by a ProbeResult with
// unknown or unreachable liveness where possible; an error is retained as a
// supervisor health degradation and does not stop probing other executions.
type Reprober interface {
	Reprobe(context.Context, Execution) (ProbeResult, error)
}

// Delivery is the bounded callback document handle kept in the local outbox.
// Payload is opaque to the supervisor and is never logged by it.
type Delivery struct {
	ID              string    `json:"id"`
	ExecutionID     string    `json:"execution_id,omitempty"`
	SubscriptionID  string    `json:"subscription_id,omitempty"`
	Attempts        int       `json:"attempts"`
	AttemptInFlight bool      `json:"attempt_in_flight,omitempty"`
	NextAttemptAt   time.Time `json:"next_attempt_at,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	Payload         []byte    `json:"-"`
}

// Outbox is the durable local callback queue adapter.  Ack, Retry, and
// DeadLetter must be idempotent against a delivery ID because a process crash
// can occur immediately before or after any one of these calls.
type Outbox interface {
	Pending(context.Context) ([]Delivery, error)
	Ack(context.Context, string) error
	Retry(context.Context, string, time.Time, string) error
	DeadLetter(context.Context, string, string) error
}

// AttemptStarter is an optional crash boundary implemented by durable
// outboxes. BeginAttempt persists the fresh envelope before transport I/O;
// when an in-flight record is replayed after a crash it returns the same
// cached attempt metadata instead of minting a new nonce.
type AttemptStarter interface {
	BeginAttempt(context.Context, string) (Delivery, error)
}

// Deliverer performs one callback attempt.  It must not mutate the outbox.
// Delivery classification is done using DeliveryError or the helper marker
// interfaces below.
type Deliverer interface {
	Deliver(context.Context, Delivery) error
}

// DeliveryError allows a callback adapter to classify HTTP/auth/destination
// failures without making the supervisor depend on a particular transport.
type DeliveryError struct {
	Err       error
	Retryable bool
	Code      int
}

func (e *DeliveryError) Error() string {
	if e == nil || e.Err == nil {
		return "delivery failed"
	}
	return e.Err.Error()
}

func (e *DeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// PermanentDeliveryError marks a delivery failure that must be dead-lettered
// immediately (for example, 401/403/404/410 or another documented 4xx).
type PermanentDeliveryError struct{ Err error }

func (e *PermanentDeliveryError) Error() string {
	if e == nil || e.Err == nil {
		return "permanent delivery failure"
	}
	return e.Err.Error()
}

func (e *PermanentDeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// RetryableDeliveryError marks a temporary delivery failure.
type RetryableDeliveryError struct{ Err error }

func (e *RetryableDeliveryError) Error() string {
	if e == nil || e.Err == nil {
		return "retryable delivery failure"
	}
	return e.Err.Error()
}

func (e *RetryableDeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// HealthState describes the managed service itself, not an execution outcome.
type HealthState string

const (
	HealthStopped  HealthState = "stopped"
	HealthStarting HealthState = "starting"
	HealthHealthy  HealthState = "healthy"
	HealthDegraded HealthState = "degraded"
	HealthStopping HealthState = "stopping"
	HealthFailed   HealthState = "failed"
)

// Health is safe to expose over the owner-only Unix socket and the optional
// authenticated read-only HTTP hook.  LastError is bounded and never includes
// callback payloads.
type Health struct {
	State                   HealthState `json:"state"`
	StartedAt               time.Time   `json:"started_at,omitempty"`
	LastCycleAt             time.Time   `json:"last_cycle_at,omitempty"`
	LastRecoveryAt          time.Time   `json:"last_recovery_at,omitempty"`
	LastOutboxAt            time.Time   `json:"last_outbox_at,omitempty"`
	NonTerminalExecutions   int         `json:"nonterminal_executions"`
	PendingDeliveries       int         `json:"pending_deliveries"`
	LastCycleReprobeErrors  int         `json:"last_cycle_reprobe_errors"`
	LastCycleDeliveryErrors int         `json:"last_cycle_delivery_errors"`
	LastError               string      `json:"last_error,omitempty"`
}

// Status is the complete bounded service projection returned by status RPC.
type Status struct {
	Running    bool      `json:"running"`
	SocketPath string    `json:"socket_path"`
	StateDir   string    `json:"state_dir"`
	Health     Health    `json:"health"`
	Now        time.Time `json:"now"`
}

// RecoveryFailure retains only the execution ID and bounded error text.
type RecoveryFailure struct {
	ExecutionID string `json:"execution_id"`
	Error       string `json:"error"`
}

// RecoveryReport records one deterministic recovery pass.
type RecoveryReport struct {
	Examined int               `json:"examined"`
	Probed   int               `json:"probed"`
	Applied  int               `json:"applied"`
	Failures []RecoveryFailure `json:"failures,omitempty"`
}

// DeliveryFailure is a bounded per-entry delivery error.
type DeliveryFailure struct {
	DeliveryID string `json:"delivery_id"`
	Attempt    int    `json:"attempt"`
	Action     string `json:"action"`
	Error      string `json:"error"`
}

// DeliveryReport records one bounded outbox drain pass.
type DeliveryReport struct {
	Examined     int               `json:"examined"`
	Attempted    int               `json:"attempted"`
	Delivered    int               `json:"delivered"`
	Retried      int               `json:"retried"`
	DeadLettered int               `json:"dead_lettered"`
	Skipped      int               `json:"skipped"`
	Failures     []DeliveryFailure `json:"failures,omitempty"`
}

// CycleReport combines recovery and delivery work from one supervisor tick.
type CycleReport struct {
	Recovery RecoveryReport `json:"recovery"`
	Delivery DeliveryReport `json:"delivery"`
}

// Authenticator authenticates a Tailnet HTTP request.  Tailnet reachability
// alone is not authentication; callers must supply a concrete authenticator.
type Authenticator interface {
	Authenticate(*http.Request) error
}

// TailnetHookConfig is intentionally opt-in.  Enabled defaults to false and
// Start never creates a listener from this config.
type TailnetHookConfig struct {
	Enabled       bool
	Authenticator Authenticator
}

// Dependencies are the authority adapters injected into a Supervisor.
type Dependencies struct {
	Executions ExecutionRepository
	Reprober   Reprober
	Outbox     Outbox
	Deliverer  Deliverer
	// Factory opens dependencies for one bounded cycle and must release any
	// underlying resources in CycleDependencies.Close before returning. This
	// is used by bbolt-backed hosts so a long-running supervisor never holds an
	// exclusive journal lock between cycles.
	Factory DependenciesFactory
}

// CycleDependencies is the authority adapter set for one recovery/delivery
// cycle. Close is called exactly once after the cycle, including cancellation
// and delivery errors. A nil Close is allowed for lock-free adapters.
type CycleDependencies struct {
	Dependencies
	Close func() error
}

// DependenciesFactory opens one bounded cycle's adapters. Implementations must
// not retain an open journal or connection after returning from Close.
type DependenciesFactory interface {
	Open(context.Context) (CycleDependencies, error)
}

// DependenciesFactoryFunc adapts a function to DependenciesFactory.
type DependenciesFactoryFunc func(context.Context) (CycleDependencies, error)

func (f DependenciesFactoryFunc) Open(ctx context.Context) (CycleDependencies, error) {
	return f(ctx)
}

// Config controls local lifecycle, polling, and bounded retry behavior.
type Config struct {
	SocketPath            string
	StateDir              string
	StatePath             string
	CycleInterval         time.Duration
	MaxDeliveryAttempts   int
	MaxDeliveriesPerCycle int
	RetryBase             time.Duration
	RetryMax              time.Duration
	ShutdownTimeout       time.Duration
	Clock                 Clock
	TailnetHook           TailnetHookConfig
}

// DefaultConfig returns conservative local-only defaults.  Tailnet HTTP is
// disabled and no host service is installed or enabled by this package.
func DefaultConfig() Config {
	return Config{
		CycleInterval:         5 * time.Second,
		MaxDeliveryAttempts:   5,
		MaxDeliveriesPerCycle: 64,
		RetryBase:             time.Second,
		RetryMax:              time.Minute,
		ShutdownTimeout:       10 * time.Second,
		Clock:                 RealClock{},
	}
}

// NewConfig is an alias kept for callers that prefer constructor naming.
func NewConfig() Config { return DefaultConfig() }
