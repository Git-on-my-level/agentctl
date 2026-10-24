package anxrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CLISender delivers registrations through the configured `anx` executable.
// It performs no HTTP of its own, hard-codes no base URL, and reads no ANX
// credentials: the subprocess owns authentication and workspace resolution.
// agentctl passes only the explicit workspace alias and optional base
// URL/config directory from its own reviewed config.
//
// Today's public surface (verified against `anx help sessions register` and
// `anx help work participants register`, 2026-10-24) accepts the native
// session registration and per-card participation. The run record itself
// (outcome, links, transcript locator, supersedes) rides in the registration
// document for the ANX-side runs extension; see docs/design/anx-run-links.md.
type CLISender struct {
	options CLIOptions
	// run is injectable for tests; production uses runAnxProcess.
	run func(ctx context.Context, argv []string, stdin []byte) (stdout []byte, err error)
}

// CLIOptions configure the anx subprocess.
type CLIOptions struct {
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
	// Code is the ANX error code when the CLI reported one.
	Code string
	// Fatal marks errors that retrying cannot fix.
	Fatal bool
	// Reason is the dead-letter reason when Fatal.
	Reason string
	// Detail is a bounded, secret-free description.
	Detail string
}

func (e *SendError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("anx delivery failed: %s: %s", e.Code, e.Detail)
	}
	return fmt.Sprintf("anx delivery failed: %s", e.Detail)
}

// Dead-letter reasons surfaced by `agentctl anx status`.
const (
	ReasonAuthRequired   = "anx_auth_required"
	ReasonRejected       = "anx_rejected"
	ReasonCardUnresolved = "card_unresolved"
	ReasonConflict       = "anx_conflict"
	ReasonNoSession      = "no_native_session_binding"
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
			return sendErr.Reason
		}
		if sendErr.Code != "" {
			return reasonForCode(sendErr.Code)
		}
	}
	return "anx_delivery_failed"
}

// Send delivers one registration through the anx CLI: the native session
// registration first, then participation for every resolved card. Delivery is
// idempotent: ANX upserts on (principal, provider, native session) plus the
// monotonic sequence carried in the registration.
func (s *CLISender) Send(ctx context.Context, registration Registration) error {
	if registration.NativeSession == nil {
		// Nothing on today's public surface is addressable without a native
		// session; the run-record extension (ANX side) closes this gap.
		return &SendError{Fatal: true, Reason: ReasonNoSession, Detail: "registration carries no native session binding"}
	}
	activity := "active"
	if registration.Phase == PhaseTerminal || terminalANXState(registration.State) {
		activity = "idle"
	}
	sessionBody, err := json.Marshal(map[string]any{
		"provider":               registration.NativeSession.Provider,
		"native_session_id":      registration.NativeSession.Fingerprint,
		"native_session_id_kind": registration.NativeSession.Kind,
		"activity":               activity,
		"sequence":               registration.Sequence,
		"host_scope":             registration.OriginHostID,
		"capabilities":           map[string]string{"resume": "unknown", "history": "unknown", "logs": "unknown"},
	})
	if err != nil {
		return &SendError{Fatal: true, Reason: ReasonRejected, Detail: "encode sessions body"}
	}
	sessionID, err := s.invoke(ctx, "sessions.register", nil, sessionBody)
	if err != nil {
		return err
	}
	// Participation references the registered session. The response shape is
	// principal-scoped; fall back to the stable fingerprint identity.
	if sessionID == "" {
		sessionID = registration.NativeSession.Fingerprint
	}
	for _, cardRef := range registration.CardRefs {
		body, err := json.Marshal(map[string]any{
			"activity":   activity,
			"sequence":   registration.Sequence,
			"session_id": sessionID,
		})
		if err != nil {
			return &SendError{Fatal: true, Reason: ReasonRejected, Detail: "encode participants body"}
		}
		if _, err := s.invoke(ctx, "work.participants.register", map[string]string{"card_ref": cardRef}, body); err != nil {
			return err
		}
	}
	return nil
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
	argv := []string{s.options.Executable, "--json", "--workspace", s.options.WorkspaceAlias}
	if s.options.BaseURL != "" {
		argv = append(argv, "--base-url", s.options.BaseURL)
	}
	if s.options.ConfigDir != "" {
		argv = append(argv, "--config-dir", s.options.ConfigDir)
	}
	argv = append(argv, "--timeout", s.options.Timeout.String(), "api", commandID)
	for _, key := range sortedKeys(params) {
		argv = append(argv, "--param", key+"="+params[key])
	}
	argv = append(argv, "--json-body", "-")
	run := s.run
	if run == nil {
		run = runAnxProcess
	}
	ctx, cancel := context.WithTimeout(ctx, s.options.Timeout)
	defer cancel()
	stdout, err := run(ctx, argv, body)
	if err != nil {
		var sendErr *SendError
		if errors.As(err, &sendErr) {
			return "", sendErr
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", &SendError{Detail: "anx executable not found on PATH"}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", &SendError{Detail: "anx invocation timed out"}
		}
		return "", &SendError{Detail: boundedString(err.Error(), 200)}
	}
	return parseSessionID(stdout)
}

// anxEnvelope is the CLI's --json output envelope.
type anxEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *anxError       `json:"error"`
}

type anxError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// parseSessionID extracts the registered session identity from a successful
// sessions.register response, tolerating additional fields.
func parseSessionID(stdout []byte) (string, error) {
	var envelope anxEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &envelope); err != nil {
		return "", &SendError{Detail: "anx returned a non-JSON envelope"}
	}
	if !envelope.OK {
		code := ""
		detail := "anx reported failure"
		if envelope.Error != nil {
			code = envelope.Error.Code
			detail = boundedString(envelope.Error.Message, 200)
		}
		return "", &SendError{Code: code, Detail: detail, Fatal: fatalANXCode(code), Reason: reasonForCode(code)}
	}
	var result struct {
		Session *struct {
			ID string `json:"id"`
		} `json:"session"`
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(envelope.Result, &result)
	switch {
	case result.Session != nil && result.Session.ID != "":
		return result.Session.ID, nil
	case result.ID != "":
		return result.ID, nil
	default:
		return result.SessionID, nil
	}
}

// fatalANXCode reports whether an ANX error code cannot be fixed by retrying.
func fatalANXCode(code string) bool {
	switch code {
	case "auth_required", "invalid_token", "forbidden", "invalid_request", "not_found", "conflict", "host_revoked", "run_identity_conflict":
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
	case "conflict", "run_identity_conflict":
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

func boundedString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// runAnxProcess executes the anx subprocess with the body on stdin.
func runAnxProcess(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = os.Environ()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ctx.Err()
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, err
		}
		return stdout.Bytes(), &SendError{Detail: boundedString(fmt.Sprintf("%s exited: %v", argv[0], err), 200)}
	}
	return stdout.Bytes(), nil
}
