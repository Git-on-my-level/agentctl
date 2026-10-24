package anxrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/route"
)

// CLISender delivers registrations through the configured `anx` executable.
// It performs no HTTP of its own, hard-codes no base URL, and reads no ANX
// credentials: the subprocess owns authentication and workspace resolution.
// agentctl passes only the explicit workspace alias and optional base
// URL/config directory from its own reviewed config.
//
// Sessions and participation are private/session metadata; an idempotent
// card message carries the public run record until ANX extends /runs.
type CLISender struct {
	// Resolve reads only private typed journal fields; candidates never persist.
	Resolve     func(context.Context, Registration) (Registration, *Repository, error)
	cards       map[string]ConfirmedCard
	cardsLoaded bool
	options     CLIOptions
	// run is injectable for tests; production uses runAnxProcess.
	run func(ctx context.Context, argv []string, stdin []byte) (stdout []byte, err error)
}

// CLIOptions configure the anx subprocess.
type CLIOptions struct {
	CacheDir    string
	AllowedOrgs []string
	// Verification is an injected authoritative catalog snapshot for tests.
	Verification *Verification
	Catalog      []route.ModelRecord
	// Executable defaults to "anx" (resolved on PATH).
	Executable string
	// WorkspaceAlias defaults to "anx". Never a hard-coded URL.
	WorkspaceAlias string
	// BaseURL and ConfigDir are optional explicit passthroughs.
	BaseURL   string
	ConfigDir string
	// Timeout bounds each subprocess invocation. Default 10s.
	Timeout time.Duration
}

func (o CLIOptions) normalized() CLIOptions {
	if strings.TrimSpace(o.Executable) == "" {
		o.Executable = "anx"
	}
	if strings.TrimSpace(o.WorkspaceAlias) == "" {
		o.WorkspaceAlias = "anx"
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	return o
}

// NewCLISender builds a production sender.
func NewCLISender(options CLIOptions) *CLISender {
	return &CLISender{options: options.normalized(), run: nil}
}

// SendError classifies delivery failures for spool retry decisions.
type SendError struct {
	NumericCode int
	// Code is the ANX error code when the CLI reported one.
	Code string
	// Fatal marks errors that retrying cannot fix.
	Fatal bool
	// Reason is the dead-letter reason when Fatal.
	Reason string
	// Detail is local-only legacy input; never persisted, printed or sent.
	Detail string
}

func (e *SendError) Error() string {
	class, code := ErrorClass(e)
	return fmt.Sprintf("%s:%d", class, code)
}

// ErrorClass is the complete diagnostic egress surface: enum + numeric code.
// Neither subprocess text nor arbitrary ANX code/message strings survive.
func ErrorClass(err error) (string, int) {
	var e *SendError
	if errors.As(err, &e) {
		class := "anx_unavailable"
		if e.Code == "protocol_error" {
			class = "anx_protocol_error"
		}
		if e.Fatal {
			class = classifyDeadReason(e)
		}
		return sanitizeReason(class), e.NumericCode
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "anx_timeout", 0
	}
	return "anx_delivery_failed", 0
}

// Dead-letter reasons surfaced by `agentctl anx status`.
const (
	ReasonAuthRequired   = "anx_auth_required"
	ReasonRejected       = "anx_rejected"
	ReasonCardUnresolved = "card_unresolved"
	ReasonConflict       = "anx_conflict"
)

// retryableSendError reports whether a failed delivery should be retried.
func retryableSendError(err error) bool {
	var sendErr *SendError
	if errors.As(err, &sendErr) {
		return !sendErr.Fatal
	}
	return true
}

// classifyDeadReason maps a fatal error to its dead-letter reason.
func classifyDeadReason(err error) string {
	var sendErr *SendError
	if errors.As(err, &sendErr) {
		if sendErr.Reason != "" {
			return sanitizeReason(sendErr.Reason)
		}
		if sendErr.Code != "" {
			return reasonForCode(sendErr.Code)
		}
	}
	return "anx_delivery_failed"
}

// Send succeeds only after ANX acknowledges every required write. Partial
// delivery is safe to replay; spool deletion is owned by Flush afterwards.
func (s *CLISender) Send(ctx context.Context, registration Registration) error {
	if _, err := Allowlisted(registration, s.options.Catalog); err != nil {
		return err
	}
	if registration.Sequence < 1 {
		return egressError(EgressFormat)
	}
	policy := Verification{AllowedOrgs: s.options.AllowedOrgs}
	if s.options.Verification != nil {
		policy = *s.options.Verification
	} else {
		if !s.cardsLoaded {
			cards, err := s.loadCards(ctx)
			if err != nil {
				return err
			}
			s.cards = cards
			s.cardsLoaded = true
		}
		policy.Cards = s.cards
	}
	if s.Resolve != nil {
		var err error
		registration, policy.Origin, err = s.Resolve(ctx, registration)
		if err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r, err := Allowlisted(registration, s.options.Catalog, policy)
	if err != nil {
		return err
	}
	if len(r.CardRefs) == 0 {
		return &SendError{Fatal: true, Reason: ReasonCardUnresolved}
	}
	activity := "active"
	if r.Phase == PhaseTerminal || terminalANXState(r.State) {
		activity = "idle"
	}
	// Native session metadata goes only to the principal-private sessions API.
	// Multica and generic/no-session runs still register their public run record.
	if r.NativeSession != nil {
		body, _ := json.Marshal(map[string]any{
			"provider": r.NativeSession.Provider, "native_session_id": r.NativeSession.Fingerprint,
			"native_session_id_kind": r.NativeSession.Kind, "activity": activity,
			"sequence": r.Sequence, "host_scope": r.OriginHostID,
			"capabilities": map[string]string{"resume": "unknown", "history": "unknown", "logs": "unknown"},
		})
		sessionID, err := s.invoke(ctx, "sessions.register", nil, body)
		if err != nil {
			return err
		}
		if !safeServerCoordinate(sessionID) {
			return &SendError{Code: "protocol_error"}
		}
		for _, card := range r.CardRefs {
			body, _ := json.Marshal(map[string]any{"activity": activity, "sequence": r.Sequence, "session_id": sessionID})
			if _, err := s.invoke(ctx, "work.participants.register", map[string]string{"card_ref": card}, body); err != nil {
				return err
			}
		}
	}
	identity, err := s.invokeResult(ctx, "auth.whoami", nil, nil)
	if err != nil {
		return err
	}
	var me struct {
		Agent struct {
			ActorID string `json:"actor_id"`
		} `json:"agent"`
	}
	if json.Unmarshal(identity, &me) != nil || !safeServerCoordinate(me.Agent.ActorID) {
		return &SendError{Code: "protocol_error"}
	}
	// A stable structured card message is today's public transport for run
	// identity/state/outcome/evidence. Native transcript coordinates stay private.
	r.NativeSession = nil
	r.BuiltAt = time.Time{}
	for _, card := range r.CardRefs {
		target := policy.Cards[card]
		text, marshalErr := json.Marshal(r)
		if marshalErr != nil {
			return egressError(EgressFormat)
		}
		// Stable across retries/re-enqueues of the same payload, including a
		// crash after ANX committed but before local acknowledgement.
		digest := sha256.Sum256(append([]byte(card+"\x00"), text...))
		body, _ := json.Marshal(map[string]any{
			"actor_id": me.Agent.ActorID, "request_key": "agentctl-run-" + hex.EncodeToString(digest[:]),
			"event": map[string]any{
				"type": "message_posted", "actor_id": me.Agent.ActorID,
				"thread_id": target.ThreadID, "thread_ref": "thread:" + target.ThreadID,
				"refs":       []string{card, "thread:" + target.ThreadID},
				"summary":    r.DurableID + " " + string(r.Phase) + " " + r.State,
				"payload":    map[string]any{"kind": "card_message", "subject_ref": card, "subject_kind": "card", "text": string(text)},
				"provenance": map[string]any{"sources": []string{"inferred"}},
			},
		})
		if _, err := s.invokeResult(ctx, "events.create", nil, body); err != nil {
			return err
		}
	}
	return nil
}

var serverCoordinate = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var serverUUID = regexp.MustCompile(`^(?:(?:agent|actor|session|sess|thread)_)?[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func safeServerCoordinate(value string) bool {
	return serverCoordinate.MatchString(value) && stringCode(value, 128, serverUUID.MatchString(value)) == 0
}

func terminalANXState(state string) bool {
	switch state {
	case StateCompleted, StateFailed, StateCancelled, StateUnknown:
		return true
	}
	return false
}

// invoke runs one anx api call with a JSON body on stdin and returns the
// session id when the response carries one.
func (s *CLISender) invoke(ctx context.Context, commandID string, params map[string]string, body []byte) (string, error) {
	result, err := s.invokeResult(ctx, commandID, params, body)
	if err != nil {
		return "", err
	}
	return sessionIDFromResult(result), nil
}

func (s *CLISender) invokeResult(ctx context.Context, commandID string, params map[string]string, body []byte) (json.RawMessage, error) {
	options := s.options.normalized()
	if code := workspaceAliasCode(options.WorkspaceAlias); code != 0 {
		return nil, egressError(code)
	}
	argv := []string{options.Executable, "--json", "--workspace", options.WorkspaceAlias}
	if options.BaseURL != "" {
		argv = append(argv, "--base-url", options.BaseURL)
	}
	if options.ConfigDir != "" {
		argv = append(argv, "--config-dir", options.ConfigDir)
	}
	argv = append(argv, "--timeout", options.Timeout.String())
	if commandID == "auth.whoami" {
		argv = append(argv, "auth", "whoami")
	} else {
		argv = append(argv, "api", commandID)
	}
	for _, key := range sortedKeys(params) {
		argv = append(argv, "--param", key+"="+params[key])
	}
	if body != nil {
		argv = append(argv, "--json-body", "-")
	}
	run := s.run
	if run == nil {
		run = runAnxProcess
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	stdout, runErr := run(ctx, argv, body)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var envelope anxEnvelope
	parseErr := json.Unmarshal(bytes.TrimSpace(stdout), &envelope)
	if parseErr == nil && !envelope.OK && envelope.Error != nil {
		code := envelope.Error.Code
		return nil, &SendError{Code: knownANXCode(code), Fatal: fatalANXCode(code), Reason: reasonForCode(code), NumericCode: processCode(runErr)}
	}
	if runErr != nil {
		var classified *SendError
		if errors.As(runErr, &classified) {
			return nil, classified
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, &SendError{NumericCode: processCode(runErr)}
	}
	if parseErr != nil || !envelope.OK {
		return nil, &SendError{Code: "protocol_error"}
	}
	return envelope.Result, nil
}

var workspaceAliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidWorkspaceAlias is shared by delivery and status: configuration strings
// may be printed only after both the exact grammar and secret-shape check.
func ValidWorkspaceAlias(value string) bool { return workspaceAliasCode(value) == 0 }

func workspaceAliasCode(value string) int {
	if code := stringCode(value, 64, false); code != 0 {
		return code
	}
	if !workspaceAliasPattern.MatchString(value) {
		return EgressFormat
	}
	return 0
}

func processCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	var send *SendError
	if errors.As(err, &send) {
		return send.NumericCode
	}
	return 0
}

func knownANXCode(code string) string {
	switch code {
	case "auth_required", "invalid_token", "forbidden", "invalid_request", "not_found", "conflict", "host_revoked", "run_identity_conflict", "run_state_regression", "sessions_unavailable", "quota_exceeded":
		return code
	default:
		return "unknown"
	}
}

// anxEnvelope is the CLI's --json output envelope.
type anxEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *anxError       `json:"error"`
}

type anxError struct {
	Code string `json:"code"`
}

func sessionIDFromResult(raw json.RawMessage) string {
	var result struct {
		Session *struct {
			ID string `json:"id"`
		} `json:"session"`
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(raw, &result)
	switch {
	case result.Session != nil && result.Session.ID != "":
		return result.Session.ID
	case result.ID != "":
		return result.ID
	default:
		return result.SessionID
	}
}

// fatalANXCode reports whether an ANX error code cannot be fixed by retrying.
func fatalANXCode(code string) bool {
	switch code {
	case "auth_required", "invalid_token", "forbidden", "invalid_request", "not_found", "conflict", "host_revoked", "run_identity_conflict", "run_state_regression", "idempotency_conflict":
		return true
	}
	return false
}

// reasonForCode maps an ANX error code onto a dead-letter reason.
func reasonForCode(code string) string {
	switch code {
	case "auth_required", "invalid_token", "forbidden", "host_revoked":
		return ReasonAuthRequired
	case "not_found":
		return ReasonCardUnresolved
	case "conflict", "run_identity_conflict", "run_state_regression", "idempotency_conflict":
		return ReasonConflict
	default:
		return ReasonRejected
	}
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for k := i; k > 0 && keys[k] < keys[k-1]; k-- {
			keys[k], keys[k-1] = keys[k-1], keys[k]
		}
	}
	return keys
}

// runAnxProcess executes the anx subprocess with the body on stdin.
func runAnxProcess(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if err := isolateANXProcess(cmd); err != nil {
		return nil, err
	}
	// Parent success, error and pipe-drain expiry all end ownership of this
	// group. Context cancellation alone cannot clean up after a reaped parent.
	defer terminateANXProcessGroup(cmd)
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = os.Environ()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &bytes.Buffer{}
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.Bytes(), ctx.Err()
	}
	return stdout.Bytes(), err
}
