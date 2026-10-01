package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// Steering re-delivers an instruction to a native session that agentctl
// already launched. Two routes exist and both are negotiated from the exact
// invocation, never from an adapter name:
//
//   - live_input: the native CLI was started in a reviewed streaming-input
//     mode and agentctl holds its stdin open. A message is written to that
//     stream and the agent takes it at its next turn boundary.
//   - interrupt_resume: the native CLI is one-shot. agentctl stops the process
//     and relaunches the same native session with the message. The in-flight
//     turn is lost, so this route is reported as degraded and requires explicit
//     caller permission.
//
// The same native resume route also continues a session after a turn has
// completed. That is a separate capability (resume): a CLI can keep a finished
// turn's context while losing an interrupted one, so a route may be resumable
// without being interruptible.
//
// A route is only registered for a CLI whose behavior was verified against a
// real installation; see docs/steering.md for the evidence.

const (
	PromptDeliveryArgv   = "argv"
	PromptDeliveryStdin  = "stdin"
	PromptDeliveryStream = "stream"
)

// liveProtocol is one native CLI's streaming-input wire format.
type liveProtocol interface {
	// EncodeUserMessage returns the bytes written to native stdin for one
	// user message, including the record terminator.
	EncodeUserMessage(message []byte) ([]byte, error)
	// Acknowledges reports whether a native stdout line confirms that one
	// previously written user message was taken into the session.
	Acknowledges(line []byte) bool
}

type steerRoute struct {
	// live returns the streaming-input protocol when argv selects it.
	live func(argv []string) (liveProtocol, bool)
	// liveArgv documents the flags live requires, for diagnostics.
	liveArgv []string
	// resume rewrites the prompt-free base argv to continue sessionID. The
	// caller attaches the message with the original prompt delivery.
	resume func(base []string, sessionID, delivery string) ([]string, error)
	// interruptible reports that a session stopped mid-turn still holds its
	// task when resumed. Without it resume is only safe after a completed turn.
	interruptible bool
	// unavailable explains why an adapter cannot be steered.
	unavailable string
	// unresumable explains why a finished session cannot be continued.
	unresumable string
}

var steerRoutes = map[string]steerRoute{
	"claude-code": {live: claudeLiveProtocol, liveArgv: []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--replay-user-messages"}, resume: claudeResumeArgv, interruptible: true},
	"codex":       {resume: codexResumeArgv, interruptible: true},
	"cursor":      {resume: cursorResumeArgv, unavailable: "cursor-agent does not persist the prompt of a turn that is interrupted, so resuming would continue without the task; cursor-agent acp is not mapped by this adapter"},
	"omp":         {resume: ompResumeArgv, unavailable: "omp does not persist a session that is interrupted during its first turn, so interrupt_resume is unverified; its --mode rpc steer command is not mapped by this adapter"},
	"zcode":       {unavailable: "zcode headless runs print one JSON document and expose no verified steering or resume route", unresumable: "zcode has no verified native session resume route"},
	"devin":       {unavailable: "devin print mode does not report its session id, so the exact session cannot be resumed; devin acp is not mapped by this adapter", unresumable: "devin print mode does not report its session id, so the exact session cannot be resumed"},
	"multica":     {unavailable: "Multica owns run input; no verified steering route exists for the current CLI", unresumable: "Multica owns issue and run continuation"},
}

func (r steerRoute) steerable() bool { return r.live != nil || (r.resume != nil && r.interruptible) }

func steerDeclaration(adapterName string) CapabilityDeclaration {
	route := steerRoutes[adapterName]
	if !route.steerable() {
		return capDecl(CapabilitySteer, CapabilityUnavailable)
	}
	decl := capDecl(CapabilitySteer, CapabilityConditional)
	deliveries := []string{}
	if route.live != nil {
		deliveries = append(deliveries, string(SteerLiveInput))
		decl.Constraints["live_input_argv"] = append([]string(nil), route.liveArgv...)
		decl.Constraints["live_input_prompt_delivery"] = PromptDeliveryStream
	}
	if route.resume != nil && route.interruptible {
		deliveries = append(deliveries, string(SteerInterruptResume))
	}
	decl.Constraints["scope"] = "owner_process"
	decl.Constraints["cross_restart"] = false
	decl.Constraints["deliveries"] = deliveries
	return decl
}

// NegotiateSteer resolves the steering route for one exact invocation. It
// performs no filesystem or network access. A static call (no argv) reports
// the declared routes as degraded: usable only once an invocation proves one.
func NegotiateSteer(manifest Manifest, argv []string, promptDelivery string, promptArgs int) Capability {
	capability := Capability{Name: CapabilitySteer, Status: CapabilityUnavailable, Source: "manifest", SemanticsVersion: SemanticsVersion, Constraints: map[string]any{}}
	route := steerRoutes[manifest.Adapter]
	if !route.steerable() {
		capability.Reason = firstNonEmpty(route.unavailable, "adapter has no verified steering route")
		return capability
	}
	declaration := steerDeclaration(manifest.Adapter)
	if len(argv) == 0 {
		capability.Status = CapabilityDegraded
		capability.Constraints = cloneMap(declaration.Constraints)
		capability.Reason = "steering is negotiated from the exact invocation"
		return capability
	}
	capability.Constraints["scope"] = "owner_process"
	capability.Constraints["cross_restart"] = false
	switch promptDelivery {
	case PromptDeliveryStream:
		if route.live != nil {
			if _, ok := route.live(argv); ok {
				capability.Status = CapabilitySupported
				capability.Constraints["delivery"] = string(SteerLiveInput)
				capability.Constraints["applies_at"] = "next_turn_boundary"
				return capability
			}
		}
		capability.Reason = "stream prompt delivery requires the adapter's live input argv"
		if route.live != nil {
			capability.Constraints["required_argv"] = append([]string(nil), route.liveArgv...)
		}
		return capability
	case PromptDeliveryArgv, PromptDeliveryStdin:
		if route.resume == nil || !route.interruptible {
			capability.Reason = firstNonEmpty(route.unavailable, "adapter has no verified native session resume route")
			return capability
		}
		base, err := steerBaseArgv(argv, promptDelivery, promptArgs)
		if err == nil {
			_, err = route.resume(base, "session", promptDelivery)
		}
		if err != nil {
			capability.Reason = err.Error()
			return capability
		}
		capability.Status = CapabilityDegraded
		capability.Constraints["delivery"] = string(SteerInterruptResume)
		capability.Constraints["in_flight_turn"] = "discarded"
		capability.Constraints["requires"] = "native_session_id"
		capability.Reason = "the native CLI is one-shot: steering stops the process and resumes the same native session with the message"
		return capability
	default:
		capability.Reason = "steering requires agentctl-managed prompt delivery (--prompt-file or --prompt-stdin)"
		return capability
	}
}

func resumeDeclaration(adapterName string) CapabilityDeclaration {
	if steerRoutes[adapterName].resume == nil {
		return capDecl(CapabilityResume, CapabilityUnavailable)
	}
	decl := capDecl(CapabilityResume, CapabilityConditional)
	decl.Constraints["after"] = "completed_turn"
	decl.Constraints["scope"] = "native_session"
	return decl
}

// NegotiateResume reports whether a later invocation can continue the native
// session this exact invocation creates, once its turn has completed. It
// performs no filesystem or network access.
func NegotiateResume(manifest Manifest, argv []string, promptDelivery string, promptArgs int) Capability {
	capability := Capability{Name: CapabilityResume, Status: CapabilityUnavailable, Source: "manifest", SemanticsVersion: SemanticsVersion, Constraints: map[string]any{}}
	route := steerRoutes[manifest.Adapter]
	if route.resume == nil {
		capability.Reason = firstNonEmpty(route.unresumable, "adapter has no verified native session resume route")
		return capability
	}
	if len(argv) == 0 {
		capability.Status = CapabilityDegraded
		capability.Constraints = cloneMap(resumeDeclaration(manifest.Adapter).Constraints)
		capability.Reason = "resume is negotiated from the exact invocation"
		return capability
	}
	if promptDelivery == "" {
		capability.Reason = "resume requires agentctl-managed prompt delivery (--prompt-file or --prompt-stdin)"
		return capability
	}
	base, err := steerBaseArgv(argv, promptDelivery, promptArgs)
	if err == nil {
		_, err = route.resume(base, "session", promptDelivery)
	}
	if err != nil {
		capability.Reason = err.Error()
		return capability
	}
	capability.Status = CapabilitySupported
	capability.Constraints["after"] = "completed_turn"
	capability.Constraints["scope"] = "native_session"
	capability.Constraints["requires"] = "native_session_id"
	return capability
}

// ContinuationArgv rewrites a prompt-free launch argv so that it continues
// sessionID. The caller attaches the next prompt with the same delivery.
func ContinuationArgv(adapterName string, base []string, promptDelivery, sessionID string) ([]string, error) {
	route := steerRoutes[adapterName]
	if route.resume == nil {
		return nil, capabilityError(CapabilityResume, firstNonEmpty(route.unresumable, "adapter has no verified native session resume route"))
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, capabilityError(CapabilityResume, "native session id is unknown")
	}
	argv, err := route.resume(append([]string(nil), base...), sessionID, promptDelivery)
	if err != nil {
		return nil, capabilityError(CapabilityResume, err.Error())
	}
	return argv, nil
}

func steerBaseArgv(argv []string, promptDelivery string, promptArgs int) ([]string, error) {
	if promptDelivery != PromptDeliveryArgv {
		return append([]string(nil), argv...), nil
	}
	if promptArgs < 1 || promptArgs >= len(argv) {
		return nil, fmt.Errorf("argv prompt position is unknown")
	}
	return append([]string(nil), argv[:len(argv)-promptArgs]...), nil
}

func argvHasFlag(argv []string, flags ...string) bool {
	for _, arg := range argv[1:] {
		if arg == "--" {
			return false
		}
		for _, flag := range flags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return true
			}
		}
	}
	return false
}

func argvFlagValue(argv []string, flag string) (string, bool) {
	for i := 1; i < len(argv); i++ {
		if argv[i] == "--" {
			break
		}
		if argv[i] == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
		if value, found := strings.CutPrefix(argv[i], flag+"="); found {
			return value, true
		}
	}
	return "", false
}

// withoutSessionSelection drops a `--resume <id>` this package added on an
// earlier turn, so an argv that already resumes can be resumed again with the
// session id the native CLI last reported.
func withoutSessionSelection(argv []string, flags ...string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		matched := false
		for _, flag := range flags {
			if argv[i] == flag && i+1 < len(argv) {
				i++
				matched = true
				break
			}
			if strings.HasPrefix(argv[i], flag+"=") {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, argv[i])
		}
	}
	return out
}

func trimTrailingDelimiter(argv []string) []string {
	if len(argv) > 1 && argv[len(argv)-1] == "--" {
		return argv[:len(argv)-1]
	}
	return argv
}

// claudeStreamJSON is Claude Code's `--input-format stream-json` protocol.
// With --replay-user-messages every user message taken from stdin is echoed
// on stdout with isReplay=true, which is the acknowledgement.
type claudeStreamJSON struct{}

func claudeLiveProtocol(argv []string) (liveProtocol, bool) {
	input, _ := argvFlagValue(argv, "--input-format")
	output, _ := argvFlagValue(argv, "--output-format")
	if input != "stream-json" || output != "stream-json" || !argvHasFlag(argv, "--replay-user-messages") || !argvHasFlag(argv, "--print", "-p") {
		return nil, false
	}
	return claudeStreamJSON{}, true
}

func (claudeStreamJSON) EncodeUserMessage(message []byte) ([]byte, error) {
	if !utf8.Valid(message) {
		return nil, fmt.Errorf("message must be valid UTF-8")
	}
	line, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": string(message)}})
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

func (claudeStreamJSON) Acknowledges(line []byte) bool {
	value, ok := decodeLine(line)
	if !ok {
		return false
	}
	replay, _ := value["isReplay"].(bool)
	return replay && firstString(value, "type") == "user"
}

func claudeResumeArgv(base []string, sessionID, delivery string) ([]string, error) {
	if delivery != PromptDeliveryArgv && delivery != PromptDeliveryStream {
		return nil, fmt.Errorf("claude session resume requires argv or stream prompt delivery")
	}
	base = withoutSessionSelection(trimTrailingDelimiter(base), "--resume", "-r")
	if argvHasFlag(base, "--resume", "-r", "--continue", "-c", "--session-id", "--fork-session", "--no-session-persistence") {
		return nil, fmt.Errorf("claude argv already selects or disables a session; it cannot be resumed")
	}
	if !argvHasFlag(base, "--print", "-p") {
		return nil, fmt.Errorf("claude session resume requires --print")
	}
	return append(append([]string(nil), trimTrailingDelimiter(base)...), "--resume", sessionID), nil
}

func cursorResumeArgv(base []string, sessionID, delivery string) ([]string, error) {
	if delivery != PromptDeliveryArgv {
		return nil, fmt.Errorf("cursor session resume requires argv prompt delivery")
	}
	base = withoutSessionSelection(trimTrailingDelimiter(base), "--resume")
	if argvHasFlag(base, "--resume", "--continue") {
		return nil, fmt.Errorf("cursor argv already selects a session; it cannot be resumed")
	}
	if !argvHasFlag(base, "--print", "-p") {
		return nil, fmt.Errorf("cursor session resume requires --print")
	}
	return append(append([]string(nil), trimTrailingDelimiter(base)...), "--resume", sessionID), nil
}

func ompResumeArgv(base []string, sessionID, delivery string) ([]string, error) {
	if delivery != PromptDeliveryArgv {
		return nil, fmt.Errorf("omp session resume requires argv prompt delivery")
	}
	base = withoutSessionSelection(trimTrailingDelimiter(base), "--resume", "-r")
	if argvHasFlag(base, "--resume", "-r", "--continue", "-c", "--no-session") {
		return nil, fmt.Errorf("omp argv already selects or disables a session; it cannot be resumed")
	}
	if !argvHasFlag(base, "--print", "-p") {
		return nil, fmt.Errorf("omp session resume requires --print")
	}
	return append(append([]string(nil), trimTrailingDelimiter(base)...), "--resume", sessionID), nil
}

// codexResumeFlags lists the `codex exec` options that `codex exec resume`
// also accepts, with whether each takes a value. Anything else fails
// negotiation so a steer never interrupts a session it cannot continue.
var codexResumeFlags = map[string]bool{
	"--json": false, "--dangerously-bypass-approvals-and-sandbox": false, "--skip-git-repo-check": false,
	"--ignore-user-config": false, "--ignore-rules": false, "--strict-config": false, "--dangerously-bypass-hook-trust": false,
	"-c": true, "--config": true, "-m": true, "--model": true, "--enable": true, "--disable": true,
	"--output-schema": true, "-o": true, "--output-last-message": true,
}

func codexResumeArgv(base []string, sessionID, delivery string) ([]string, error) {
	if len(base) < 2 || base[1] != "exec" {
		return nil, fmt.Errorf("codex session resume requires `codex exec`")
	}
	out := []string{base[0], "exec", "resume"}
	rest := base[2:]
	// An argv that already resumes carries the earlier session id as its one
	// positional; drop both so the id the native CLI last reported is used.
	resuming := len(rest) != 0 && rest[0] == "resume"
	if resuming {
		rest = rest[1:]
	}
	if delivery == PromptDeliveryStdin && len(rest) != 0 && rest[len(rest)-1] == "-" {
		rest = rest[:len(rest)-1]
	}
	if len(rest) != 0 && rest[len(rest)-1] == "--" {
		rest = rest[:len(rest)-1]
	}
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		name, inline, hasInline := strings.Cut(arg, "=")
		if !strings.HasPrefix(arg, "--") {
			name, hasInline = arg, false
		}
		if name == "--sandbox" || name == "-s" {
			value := inline
			if !hasInline {
				if i+1 >= len(rest) {
					return nil, fmt.Errorf("codex --sandbox requires a value")
				}
				i++
				value = rest[i]
			}
			// `codex exec resume` has no --sandbox flag; the config key is the
			// same policy.
			out = append(out, "-c", `sandbox_mode="`+value+`"`)
			continue
		}
		if resuming && !strings.HasPrefix(arg, "-") {
			resuming = false
			continue
		}
		takesValue, known := codexResumeFlags[name]
		if !known {
			return nil, fmt.Errorf("codex argv element %q is not accepted by `codex exec resume`", boundedString(arg, 64))
		}
		out = append(out, arg)
		if takesValue && !hasInline {
			if i+1 >= len(rest) {
				return nil, fmt.Errorf("codex %s requires a value", name)
			}
			i++
			out = append(out, rest[i])
		}
	}
	out = append(out, sessionID)
	if delivery == PromptDeliveryStdin {
		out = append(out, "-")
	}
	return out, nil
}

// liveInputBacklog bounds the messages waiting to be written to native stdin.
const liveInputBacklog = 16

// liveSession is the held-open native stdin of a streaming-input launch.
// pending counts user messages accepted for the stream but not yet
// acknowledged by the native CLI. A native terminal record is the end of the
// session only when nothing is pending; otherwise it is the end of one turn
// and another turn follows.
type liveSession struct {
	protocol liveProtocol
	mu       sync.Mutex
	stdin    io.WriteCloser
	queue    [][]byte
	writing  bool
	sent     int
	pending  int
	closed   bool
	broken   bool
}

// send queues one user message and returns its input sequence: 1 is the
// launch prompt, later ones are steering messages. A native CLI reads its
// input only at a turn boundary, so a write can block on a full pipe for as
// long as a turn lasts. A writer goroutine therefore performs the write and
// send never blocks its caller, which is the owner's observation loop.
func (l *liveSession) send(message []byte) (int, error) {
	encoded, err := l.protocol.EncodeUserMessage(message)
	if err != nil {
		return 0, invalidRequest(err.Error())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, &AdapterError{Code: ErrInvalidState, Message: "native session already reported its final result"}
	}
	if l.broken {
		return 0, &AdapterError{Code: ErrExecutionFailed, Message: "native live input stream rejected an earlier message"}
	}
	if len(l.queue) >= liveInputBacklog {
		return 0, &AdapterError{Code: ErrInvalidState, Message: "native live input stream has a backlog of unwritten messages", Retryable: true}
	}
	l.sent++
	l.pending++
	l.queue = append(l.queue, encoded)
	if !l.writing {
		l.writing = true
		go l.drain()
	}
	return l.sent, nil
}

// drain writes queued messages in order. A failed write means the native CLI
// closed its input: nothing still queued can be taken, so those messages stop
// counting as pending and the next native terminal record ends the session.
func (l *liveSession) drain() {
	for {
		l.mu.Lock()
		if len(l.queue) == 0 || l.closed {
			l.queue, l.writing = nil, false
			l.mu.Unlock()
			return
		}
		next := l.queue[0]
		l.queue = l.queue[1:]
		l.mu.Unlock()
		if _, err := l.stdin.Write(next); err != nil {
			l.mu.Lock()
			l.pending -= 1 + len(l.queue)
			if l.pending < 0 {
				l.pending = 0
			}
			l.queue, l.writing, l.broken = nil, false, true
			l.mu.Unlock()
			return
		}
	}
}

func (l *liveSession) acknowledge(line []byte) bool {
	if !l.protocol.Acknowledges(line) {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending > 0 {
		l.pending--
	}
	return true
}

// finishTurn is called for a native terminal record. It returns true and
// closes native stdin when the record ends the session.
func (l *liveSession) finishTurn() bool {
	l.mu.Lock()
	if l.pending > 0 || l.closed {
		final := l.closed
		l.mu.Unlock()
		return final
	}
	l.closed = true
	l.mu.Unlock()
	_ = l.stdin.Close()
	return true
}

func (l *liveSession) close() {
	l.mu.Lock()
	already := l.closed
	l.closed = true
	l.mu.Unlock()
	if !already {
		_ = l.stdin.Close()
	}
}
