package adapter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultOutputLimit         = 2 << 20
	nativeObservationFreshness = 15 * time.Second
)

var (
	secretAssignmentPattern = regexp.MustCompile(`(?i)\b(api[_-]?key|authorization|bearer|password|secret|token)(\s*[:=]?\s+|\s*=\s*)\S+`)
	urlCredentialPattern    = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)
)

type parsedObservation struct {
	SessionID      string
	BackendVersion string
	Cursor         string
	// CursorAuthority marks a cursor supplied by the native authority rather
	// than the adapter's local observation counter. Multica event pages use
	// this to preserve event sequence/source positions across polling calls.
	CursorAuthority  bool
	SourcePosition   string
	Kind             string
	SourceState      string
	State            State
	Liveness         Liveness
	Terminal         bool
	Success          bool
	Summary          string
	Content          string
	ContentType      string
	ContentSource    string
	ContentTruncated bool
	Error            string
	OccurredAt       *time.Time
	Data             map[string]any
	Page             *parsedPage
}

type parsedPage struct {
	Observations []parsedObservation
	NextCursor   string
	Scanned      int
}

func safeFailureDiagnostic(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = secretAssignmentPattern.ReplaceAllString(value, "$1 [REDACTED]")
	value = urlCredentialPattern.ReplaceAllString(value, "$1[REDACTED]@")
	return boundedString(value, 1024)
}

type outputParser interface {
	Parse(line []byte, stderr bool) parsedObservation
	Name() string
}

type processRecord struct {
	mu               sync.Mutex
	cmd              *exec.Cmd
	parser           outputParser
	ref              SourceRef
	binding          SourceBinding
	startedAt        time.Time
	updatedAt        time.Time
	done             chan struct{}
	waitErr          error
	exitCode         *int
	observations     []parsedObservation
	events           []Event
	result           *Result
	finalContent     string
	contentType      string
	contentSource    string
	contentTruncated bool
	lastError        string
	stderrDiagnostic string
	parseWarnings    []string
	stdoutBytes      int
	stderrBytes      int
	maxOutput        int
	cancelled        bool
	pipes            sync.WaitGroup
	resultPath       string
	page             *parsedPage
	wholeStdout      bool
}

func (p *processRecord) ingest(line []byte, stderr bool) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	p.mu.Lock()
	if p.maxOutput <= 0 {
		p.maxOutput = defaultOutputLimit
	}
	if stderr {
		p.stderrBytes += len(line)
	} else {
		p.stdoutBytes += len(line)
	}
	if p.stdoutBytes+p.stderrBytes > p.maxOutput {
		if len(p.parseWarnings) == 0 || p.parseWarnings[len(p.parseWarnings)-1] != "structured output exceeded bounded capture limit" {
			p.parseWarnings = append(p.parseWarnings, "structured output exceeded bounded capture limit")
		}
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	obs := p.parser.Parse(line, stderr)
	if stderr {
		p.mu.Lock()
		p.stderrDiagnostic = safeFailureDiagnostic(string(line))
		p.mu.Unlock()
	}
	if obs.Page != nil {
		p.mu.Lock()
		p.page = obs.Page
		p.mu.Unlock()
		for _, child := range obs.Page.Observations {
			p.ingestObservation(child)
		}
		return
	}
	p.ingestObservation(obs)
}

func (p *processRecord) ingestObservation(obs parsedObservation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if obs.SessionID != "" {
		p.ref.OpaqueID = obs.SessionID
		p.binding = p.ref.Binding()
	}
	if obs.BackendVersion != "" {
		p.ref.Endpoint = obs.BackendVersion
	}
	if obs.Content != "" {
		p.finalContent = obs.Content
		p.contentType = firstNonEmpty(obs.ContentType, "text/plain")
		p.contentSource = firstNonEmpty(obs.ContentSource, "terminal_result")
		p.contentTruncated = obs.ContentTruncated
	}
	if obs.Error != "" {
		p.lastError = obs.Error
	}
	if obs.Kind != "" || obs.State != "" || obs.SourceState != "" || obs.Error != "" {
		p.observations = append(p.observations, obs)
	}
	if obs.Kind != "" {
		seq := int64(len(p.events) + 1)
		payload := map[string]any{}
		for k, v := range obs.Data {
			payload[k] = v
		}
		cursor := obs.Cursor
		if cursor == "" && !obs.CursorAuthority {
			cursor = strconv.FormatInt(seq, 10)
		}
		e := Event{
			Sequence:       seq,
			Cursor:         firstNonEmpty(obs.Cursor, cursor),
			SourcePosition: obs.SourcePosition,
			Kind:           obs.Kind,
			State:          obs.State,
			SourceState:    obs.SourceState,
			OccurredAt:     obs.OccurredAt,
			ObservedAt:     time.Now().UTC(),
			Ordering:       "source",
			Payload:        payload,
		}
		e.DedupeKey = Fingerprint(p.ref.Kind, p.ref.OpaqueID, obs.SourceState, string(marshalStable(payload)))
		p.events = append(p.events, e)
	}
	if obs.Terminal {
		content := firstNonEmpty(obs.Content, p.finalContent)
		contentType := firstNonEmpty(obs.ContentType, p.contentType)
		contentSource := firstNonEmpty(obs.ContentSource, p.contentSource)
		data := cloneMap(obs.Data)
		if data["diagnostic_code"] == "empty_terminal_result" && content != "" {
			data["result_content_source"] = "assistant_message_fallback"
			contentSource = "assistant"
		} else if content != "" {
			data["result_content_source"] = firstNonEmpty(contentSource, "terminal_result")
		}
		result := &Result{Success: obs.Success, State: obs.State, Summary: firstNonEmpty(obs.Summary, boundedString(content, 2048)), Content: content, ContentType: contentType, ContentTruncated: obs.ContentTruncated || p.contentTruncated, Error: obs.Error, SessionRef: p.ref, Data: data}
		p.result = result
	}
	p.updatedAt = time.Now().UTC()
}

func (p *processRecord) finish(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waitErr = err
	if p.exitCode == nil {
		code := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok && e.ProcessState != nil {
				code = e.ProcessState.ExitCode()
			} else {
				code = -1
			}
		}
		p.exitCode = &code
	}
	if p.result == nil && p.resultPath != "" {
		if data, readErr := os.ReadFile(p.resultPath); readErr == nil && len(data) <= p.maxOutput {
			obs := p.parser.Parse(bytes.TrimSpace(data), false)
			if obs.Terminal {
				content := firstNonEmpty(obs.Content, p.finalContent)
				data := cloneMap(obs.Data)
				if content != "" {
					data["result_content_source"] = firstNonEmpty(obs.ContentSource, p.contentSource, "terminal_result")
				}
				p.result = &Result{Success: obs.Success, State: obs.State, Summary: obs.Summary, Content: content, ContentType: firstNonEmpty(obs.ContentType, p.contentType), ContentTruncated: obs.ContentTruncated || p.contentTruncated, Error: obs.Error, ExitCode: p.exitCode, SessionRef: p.ref, Data: data}
			}
		} else if readErr != nil && !os.IsNotExist(readErr) {
			p.parseWarnings = append(p.parseWarnings, "result file could not be read")
		}
	}
	if p.result == nil {
		if p.cancelled {
			p.result = &Result{Success: false, State: StateCancelled, Error: "native process cancelled", ExitCode: p.exitCode, SessionRef: p.ref}
		} else if p.exitCode != nil && *p.exitCode != 0 {
			p.result = &Result{Success: false, State: StateFailed, Error: firstNonEmpty(p.lastError, p.stderrDiagnostic, "native process exited unsuccessfully"), ExitCode: p.exitCode, SessionRef: p.ref}
		} else {
			// A zero exit code does not establish domain success. Native adapters
			// require an explicit terminal result in their structured stream.
			p.result = &Result{Success: false, State: StateFailed, Error: firstNonEmpty(p.lastError, p.stderrDiagnostic, "native process exited without an explicit result"), ExitCode: p.exitCode, SessionRef: p.ref}
		}
	}
	p.updatedAt = time.Now().UTC()
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

type nativeConfig struct {
	Manifest      Manifest
	Binary        string
	Parser        outputParser
	StatusArgs    func(SourceRef) []string
	EventsArgs    func(SourceRef, string) []string
	ResultArgs    func(SourceRef) []string
	ResumeArgs    func(SourceRef, []string) []string
	CancelArgs    func(SourceRef, string) []string
	PollStatus    bool
	OutputLimit   int
	LaunchKind    string
	TransformArgv func([]string) []string
	// WholeStdout asks the adapter to parse stdout as one bounded JSON
	// document after EOF. Use this for finite CLIs that pretty-print a page
	// across several lines rather than emitting JSONL observations.
	WholeStdout bool
}

// NativeAdapter implements the common process/session behavior. Constructors
// below provide reviewed defaults for each supported runtime.
type NativeAdapter struct {
	config nativeConfig
	mu     sync.Mutex
	byKey  map[string]*processRecord
	byPID  map[int]*processRecord
}

func newNativeAdapter(config nativeConfig) *NativeAdapter {
	if config.OutputLimit <= 0 {
		config.OutputLimit = defaultOutputLimit
	}
	if config.LaunchKind == "" {
		config.LaunchKind = config.Manifest.Adapter + "_session"
	}
	return &NativeAdapter{config: config, byKey: map[string]*processRecord{}, byPID: map[int]*processRecord{}}
}

func (a *NativeAdapter) Name() string       { return a.config.Manifest.Adapter }
func (a *NativeAdapter) Manifest() Manifest { return a.config.Manifest }

func (a *NativeAdapter) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	started := time.Now().UTC()
	path := req.Executable
	if path == "" {
		path = a.config.Binary
	}
	if path == "" {
		return ProbeResult{}, dependencyError("adapter has no executable discovery rule", nil)
	}
	resolved, err := resolveExecutable(path)
	if err != nil {
		return ProbeResult{}, dependencyError("native executable is unavailable", err)
	}
	backendVersion := ""
	if len(a.config.Manifest.ProbeActions) != 0 {
		action := a.config.Manifest.ProbeActions[0]
		if action.SideEffectClass != "read_only" || action.WritesCache {
			return ProbeResult{}, capabilityError(CapabilitySnapshot, "declared probe action is not read-only")
		}
		probeArgs := append([]string(nil), action.Argv...)
		if len(probeArgs) == 0 {
			return ProbeResult{}, invalidRequest("declared probe action has empty argv")
		}
		for i, arg := range probeArgs {
			if arg == "{executable}" {
				probeArgs[i] = resolved
			}
		}
		probeTimeout := req.Timeout
		if probeTimeout <= 0 {
			probeTimeout = time.Duration(action.TimeoutSeconds) * time.Second
			if probeTimeout <= 0 {
				probeTimeout = 5 * time.Second
			}
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		cmd := exec.CommandContext(probeCtx, probeArgs[0], probeArgs[1:]...)
		cmd.Env = os.Environ()
		var out bytes.Buffer
		cmd.Stdout = &limitedWriter{w: &out, n: 8192}
		cmd.Stderr = &limitedWriter{w: &out, n: 8192}
		cmdErr := cmd.Run()
		if cmdErr != nil {
			if probeCtx.Err() != nil {
				return ProbeResult{}, nativeContextError(probeCtx, "native version probe")
			}
			return ProbeResult{}, nativeProbeCommandError(out.String(), cmdErr)
		}
		backendVersion = firstLine(out.String())
	}
	digest, digestErr := executableDigest(resolved)
	if digestErr != nil {
		digest = ""
	}
	capabilities := make([]Capability, 0, len(a.config.Manifest.Capabilities))
	for _, declaration := range a.config.Manifest.Capabilities {
		status := declaration.Implementation
		if status == CapabilityConditional {
			status = CapabilityDegraded
		}
		if status == "" {
			status = CapabilitySupported
		}
		capabilities = append(capabilities, Capability{Name: declaration.Name, Status: status, Source: "manifest", SemanticsVersion: declaration.SemanticsVersion, Constraints: cloneMap(declaration.Constraints)})
	}
	scope := Fingerprint(a.Name(), backendVersion, digest, req.Profile, req.Endpoint, req.Workspace)
	return ProbeResult{AdapterVersion: a.config.Manifest.AdapterVersion, BackendVersion: backendVersion, Executable: resolved, ExecutableDigest: digest, ScopeFingerprint: scope, ProbedAt: started, FreshFor: time.Minute, ReadOnly: true, Capabilities: capabilities}, nil
}

func nativeProbeCommandError(output string, cause error) error {
	lower := strings.ToLower(output)
	if strings.Contains(lower, "login keychain is locked") {
		return &AdapterError{
			Code:      ErrAuthenticationRequired,
			Message:   "native version probe requires an unlocked macOS login keychain",
			Retryable: true,
			Details: map[string]any{
				"attention_kind":  "authentication",
				"diagnostic_code": "macos_login_keychain_locked",
				"remediation":     "unlock the login keychain in an interactive macOS user session, then retry",
			},
			Cause: cause,
		}
	}
	details := map[string]any{"diagnostic_code": "native_version_probe_failed"}
	var exitErr *exec.ExitError
	if errors.As(cause, &exitErr) && exitErr.ProcessState != nil {
		details["native_exit_code"] = exitErr.ProcessState.ExitCode()
	}
	return &AdapterError{Code: ErrDependencyUnavailable, Message: "native version probe failed", Retryable: true, Details: details, Cause: cause}
}

func resolveExecutable(name string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		st, err := os.Stat(name)
		if err != nil {
			return "", err
		}
		if st.IsDir() {
			return "", fmt.Errorf("executable path is a directory")
		}
		return name, nil
	}
	return exec.LookPath(name)
}

func executableDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (a *NativeAdapter) Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return LaunchResult{}, invalidRequest("launch requires a non-empty argv")
	}
	if req.Context != nil && req.Context.Required && !a.config.Manifest.ContextInjection.Guaranteed {
		return LaunchResult{}, capabilityError(CapabilityContextInjection, "required context cannot be guaranteed by this native adapter")
	}
	runCtx := ctx
	var timeoutCancel context.CancelFunc
	started := false
	if req.Timeout > 0 {
		runCtx, timeoutCancel = context.WithTimeout(ctx, req.Timeout)
		defer func() {
			// The timeout context remains owned by a StartOnly child; its timer
			// cancels the child at the requested deadline. All non-start-only
			// paths release it when this call returns.
			if timeoutCancel != nil && (!req.StartOnly || !started) {
				timeoutCancel()
			}
		}()
	}
	argv := append([]string(nil), req.Argv...)
	if a.config.TransformArgv != nil {
		argv = a.config.TransformArgv(argv)
	}
	if len(argv) == 0 || argv[0] == "" {
		return LaunchResult{}, invalidRequest("launch argv transform returned an empty argv")
	}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = append(os.Environ(), req.Env...)
	if req.Context != nil {
		// Handles are intentionally opaque; the child is not assumed to read
		// them. Required delivery was checked above.
		if req.Context.Path != "" {
			cmd.Env = append(cmd.Env, "AGENTCTL_CONTEXT_PATH="+req.Context.Path)
		}
		if req.Context.ArtifactRef != "" {
			cmd.Env = append(cmd.Env, "AGENTCTL_CONTEXT="+req.Context.ArtifactRef)
		}
		if req.Context.Instruction != "" {
			cmd.Env = append(cmd.Env, "AGENTCTL_EXECUTION="+Fingerprint(a.Name(), req.Context.Instruction))
		}
	}
	prepareProcess(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return LaunchResult{}, dependencyError("cannot capture native stdout", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return LaunchResult{}, dependencyError("cannot capture native stderr", err)
	}
	if err := cmd.Start(); err != nil {
		// exec.CommandContext reports a context error from Start when the
		// caller is cancelled before the child is accepted. That is an
		// interruption, not an unavailable executable or other dependency.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return LaunchResult{}, nativeContextError(runCtx, "native process launch")
		}
		return LaunchResult{}, dependencyError("native process failed to start", err)
	}
	started = true
	ref := SourceRef{Adapter: a.Name(), Kind: a.config.LaunchKind, OpaqueID: strconv.Itoa(cmd.Process.Pid), PID: cmd.Process.Pid}
	record := &processRecord{cmd: cmd, parser: a.config.Parser, ref: ref, binding: ref.Binding(), startedAt: time.Now().UTC(), updatedAt: time.Now().UTC(), done: make(chan struct{}), maxOutput: a.config.OutputLimit, resultPath: req.ResultPath, wholeStdout: a.config.WholeStdout}
	a.mu.Lock()
	a.byPID[cmd.Process.Pid] = record
	a.byKey[record.binding.Fingerprint] = record
	a.mu.Unlock()
	record.pipes.Add(2)
	go a.readPipe(record, stdout, false)
	go a.readPipe(record, stderr, true)
	go func() {
		// StdoutPipe/StderrPipe require all reads to complete before Wait. Calling
		// Wait concurrently may close a pipe while the final structured document
		// is still being drained, producing a false malformed/no-page result.
		record.pipes.Wait()
		err := cmd.Wait()
		if runCtx.Err() != nil {
			record.mu.Lock()
			record.cancelled = true
			record.mu.Unlock()
		}
		record.finish(err)
	}()
	window := req.DiscoveryWindow
	if window <= 0 {
		window = 250 * time.Millisecond
	}
	timer := time.NewTimer(window)
	select {
	case <-record.done:
		if !timer.Stop() {
			<-timer.C
		}
	case <-timer.C:
	case <-runCtx.Done():
		if !timer.Stop() {
			<-timer.C
		}
	}
	if !req.StartOnly {
		// A normal daemonless launch owns the child until it exits. This keeps
		// stdout/stderr pipes attached and guarantees Wait reaps the process
		// before the caller can return. Callers that intentionally supervise a
		// live process in-process must opt into StartOnly explicitly.
		if _, err := a.waitRecord(runCtx, record, true); err != nil {
			return LaunchResult{Session: a.session(record), Result: a.currentResult(record)}, err
		}
	}
	return LaunchResult{Session: a.session(record), Result: a.currentResult(record)}, nil
}

func nativeContextError(ctx context.Context, operation string) *AdapterError {
	err := ctx.Err()
	if errors.Is(err, context.DeadlineExceeded) {
		return &AdapterError{Code: ErrTimeout, Message: operation + " timed out", Retryable: true, Cause: err}
	}
	return &AdapterError{Code: ErrExecutionCancelled, Message: operation + " cancelled", Retryable: false, Cause: err}
}

func (a *NativeAdapter) readPipe(record *processRecord, r io.Reader, stderr bool) {
	defer record.pipes.Done()
	if record.wholeStdout && !stderr {
		limit := record.maxOutput
		if limit <= 0 {
			limit = defaultOutputLimit
		}
		document, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
		if err != nil {
			record.mu.Lock()
			record.parseWarnings = append(record.parseWarnings, "structured output stream was truncated: "+err.Error())
			record.mu.Unlock()
			return
		}
		record.ingest(document, false)
		return
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		record.ingest(scanner.Bytes(), stderr)
	}
	if err := scanner.Err(); err != nil {
		record.mu.Lock()
		record.parseWarnings = append(record.parseWarnings, "structured output stream was truncated: "+err.Error())
		record.mu.Unlock()
	}
}

func (a *NativeAdapter) recordFor(ref SourceRef) *processRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ref.Fingerprint != "" {
		if record := a.byKey[ref.Fingerprint]; record != nil {
			return record
		}
	}
	if b := ref.Binding(); b.Fingerprint != "" {
		if record := a.byKey[b.Fingerprint]; record != nil {
			return record
		}
	}
	if ref.PID != 0 {
		return a.byPID[ref.PID]
	}
	if ref.OpaqueID != "" {
		for _, record := range a.byPID {
			if record.ref.OpaqueID == ref.OpaqueID {
				return record
			}
		}
	}
	return nil
}

func (a *NativeAdapter) session(record *processRecord) Session {
	record.mu.Lock()
	defer record.mu.Unlock()
	state, live := StateRunning, LivenessAlive
	if record.result != nil {
		state = record.result.State
		live = LivenessExited
	} else {
		select {
		case <-record.done:
			state, live = StateFailed, LivenessExited
		default:
		}
	}
	if record.cancelled {
		state, live = StateCancelled, LivenessExited
	}
	return Session{Ref: record.ref, Binding: record.binding, State: state, Liveness: live, ExitCode: cloneInt(record.exitCode), StartedAt: record.startedAt, UpdatedAt: record.updatedAt, Observation: Observation{Source: "native_stream", Integrity: "verified", ObservedAt: record.updatedAt, FreshFor: nativeObservationFreshness}}
}

func (a *NativeAdapter) currentResult(record *processRecord) *Result {
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.result == nil {
		return nil
	}
	v := *record.result
	v.SessionRef = record.ref
	v.Data = cloneMap(record.result.Data)
	return &v
}

// Wait blocks until a same-process session has a terminal result and confirms
// that the child has been reaped. It never discovers sessions from ambient
// native history after a process restart.
func (a *NativeAdapter) Wait(ctx context.Context, ref SourceRef) (Result, error) {
	if ref.Empty() {
		return Result{}, invalidRequest("wait requires a source reference")
	}
	record := a.recordFor(ref)
	if record == nil {
		return Result{}, &AdapterError{Code: ErrNotFound, Message: "native session is not known to this adapter"}
	}
	if _, err := a.waitRecord(ctx, record, true); err != nil {
		return Result{}, err
	}
	result := a.currentResult(record)
	if result == nil {
		return Result{}, &AdapterError{Code: ErrExecutionUnknown, Message: "native process ended without a result", Retryable: true}
	}
	return *result, nil
}

func (a *NativeAdapter) waitRecord(ctx context.Context, record *processRecord, killOnCancel bool) (*Result, error) {
	select {
	case <-record.done:
		return a.currentResult(record), nil
	case <-ctx.Done():
		if killOnCancel {
			a.cancelRecord(record)
		}
		// CommandContext/Process.Kill closes the child; wait for the existing
		// waiter and pipe readers rather than returning with an orphan.
		<-record.done
		code, message := ErrExecutionCancelled, "native process cancelled while waiting"
		if ctx.Err() == context.DeadlineExceeded {
			code, message = ErrTimeout, "native process timed out while waiting"
		}
		return a.currentResult(record), &AdapterError{Code: code, Message: message, Retryable: code == ErrTimeout, Cause: ctx.Err()}
	}
}

func (a *NativeAdapter) cancelRecord(record *processRecord) {
	record.mu.Lock()
	record.cancelled = true
	cmd := record.cmd
	record.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = killProcess(cmd)
	}
}

func (a *NativeAdapter) Attach(ctx context.Context, req AttachRequest) (Attachment, error) {
	if req.Ref.Empty() {
		return Attachment{}, invalidRequest("attach requires a source reference")
	}
	if record := a.recordFor(req.Ref); record != nil {
		return Attachment{Session: a.session(record)}, nil
	}
	return Attachment{}, &AdapterError{Code: ErrNotFound, Message: "native session is not known to this adapter", Details: map[string]any{"fingerprint": req.Ref.Fingerprint}}
}

func (a *NativeAdapter) Snapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error) {
	if req.Ref.Empty() {
		return Snapshot{}, invalidRequest("snapshot requires a source reference")
	}
	if record := a.recordFor(req.Ref); record != nil {
		return Snapshot{Session: a.session(record)}, nil
	}
	if a.config.StatusArgs == nil {
		return Snapshot{}, capabilityError(CapabilitySnapshot, "native status route is not verified")
	}
	return a.snapshotByCommand(ctx, req.Ref, req.Poll)
}

func (a *NativeAdapter) snapshotByCommand(ctx context.Context, ref SourceRef, poll PollOptions) (Snapshot, error) {
	poll = poll.normalized()
	var last Snapshot
	for attempt := 0; attempt < poll.Attempts; attempt++ {
		args := a.config.StatusArgs(ref)
		launch, err := a.runOneShot(ctx, args, ref)
		if err != nil {
			return Snapshot{}, err
		}
		last = Snapshot{Session: launch.Session}
		if isTerminal(launch.Session.State) || attempt+1 == poll.Attempts {
			return last, nil
		}
		if err := waitPoll(ctx, pollDelay(poll, attempt)); err != nil {
			return Snapshot{}, err
		}
	}
	return last, nil
}

func (a *NativeAdapter) Events(ctx context.Context, req EventsRequest) ([]Event, error) {
	if req.Ref.Empty() {
		return nil, invalidRequest("events requires a source reference")
	}
	if record := a.recordFor(req.Ref); record != nil {
		return record.eventsAfter(req.Cursor), nil
	}
	if a.config.EventsArgs != nil {
		args := a.config.EventsArgs(req.Ref, req.Cursor)
		launch, err := a.runOneShot(ctx, args, req.Ref)
		if err != nil {
			return nil, err
		}
		if record := a.recordFor(launch.Session.Ref); record != nil {
			return record.eventsAfter(req.Cursor), nil
		}
		return nil, nil
	}
	if a.config.PollStatus {
		// Polling only reconstructs observation order; it does not advertise a
		// durable stream or cross-restart history.
		snap, err := a.snapshotByCommand(ctx, req.Ref, req.Poll)
		if err != nil {
			return nil, err
		}
		e := Event{Sequence: 1, Cursor: "1", Kind: "health", State: snap.Session.State, SourceState: string(snap.Session.State), ObservedAt: time.Now().UTC(), Ordering: "observation", Payload: map[string]any{"source": "bounded_poll"}}
		e.DedupeKey = Fingerprint(req.Ref.Fingerprint, "poll", string(snap.Session.State))
		return []Event{e}, nil
	}
	return nil, capabilityError(CapabilityEvents, "native event stream and status fallback are unavailable")
}

func (p *processRecord) eventsAfter(cursor string) []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	start := 0
	if cursor != "" {
		if n, err := strconv.Atoi(cursor); err == nil {
			start = n
		}
	}
	if start < 0 {
		start = 0
	}
	if start >= len(p.events) {
		return nil
	}
	out := make([]Event, len(p.events)-start)
	copy(out, p.events[start:])
	return out
}

func (a *NativeAdapter) Result(ctx context.Context, req ResultRequest) (Result, error) {
	if req.Ref.Empty() {
		return Result{}, invalidRequest("result requires a source reference")
	}
	if record := a.recordFor(req.Ref); record != nil {
		if result := a.currentResult(record); result != nil {
			return *result, nil
		}
		return Result{Success: false, State: StateRunning, SessionRef: record.ref}, nil
	}
	if a.config.ResultArgs == nil {
		return Result{}, capabilityError(CapabilityResult, "native result route is not verified")
	}
	launch, err := a.runOneShot(ctx, a.config.ResultArgs(req.Ref), req.Ref)
	if err != nil {
		return Result{}, err
	}
	if launch.Result == nil {
		return Result{}, &AdapterError{Code: ErrExecutionUnknown, Message: "native result did not include an explicit terminal result", Retryable: true}
	}
	return *launch.Result, nil
}

func (a *NativeAdapter) Resume(ctx context.Context, req ResumeRequest) (LaunchResult, error) {
	if req.Ref.Empty() {
		return LaunchResult{}, invalidRequest("resume requires a source reference")
	}
	if a.config.ResumeArgs == nil {
		return LaunchResult{}, capabilityError(CapabilityResume, "native resume route is not verified")
	}
	args := a.config.ResumeArgs(req.Ref, req.Argv)
	if len(args) == 0 {
		return LaunchResult{}, invalidRequest("native resume route returned empty argv")
	}
	return a.Launch(ctx, LaunchRequest{Argv: args, Context: req.Context, Timeout: req.Timeout, DiscoveryWindow: req.DiscoveryWindow})
}

func (a *NativeAdapter) Cancel(ctx context.Context, req CancelRequest) error {
	if req.Ref.Empty() {
		return invalidRequest("cancel requires a source reference")
	}
	if record := a.recordFor(req.Ref); record != nil {
		select {
		case <-record.done:
			return nil
		default:
		}
		a.cancelRecord(record)
		<-record.done
		return nil
	}
	if a.config.CancelArgs == nil {
		return capabilityError(CapabilityCancel, "native cancellation route is not verified")
	}
	_, err := a.runOneShot(ctx, a.config.CancelArgs(req.Ref, req.Signal), req.Ref)
	return err
}

func (a *NativeAdapter) runOneShot(ctx context.Context, argv []string, ref SourceRef) (LaunchResult, error) {
	if len(argv) == 0 || argv[0] == "" {
		return LaunchResult{}, invalidRequest("native command route returned empty argv")
	}
	return a.Launch(ctx, LaunchRequest{Argv: argv, DiscoveryWindow: 250 * time.Millisecond, StartOnly: false})
}

func isTerminal(state State) bool {
	return state == StateCompleted || state == StateFailed || state == StateCancelled || state == StateOrphaned
}

func waitPoll(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return &AdapterError{Code: ErrTimeout, Message: "bounded poll cancelled", Retryable: true, Cause: ctx.Err()}
	case <-t.C:
		return nil
	}
}

func pollDelay(options PollOptions, attempt int) time.Duration {
	options = options.normalized()
	delay := options.Interval
	for i := 0; i < attempt && delay < options.MaxInterval; i++ {
		if delay > options.MaxInterval/2 {
			delay = options.MaxInterval
			break
		}
		delay *= 2
	}
	if delay > options.MaxInterval {
		return options.MaxInterval
	}
	return delay
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return len(p), nil
	}
	if len(p) > w.n {
		p = p[:w.n]
	}
	n, err := w.w.Write(p)
	w.n -= n
	if w.n <= 0 {
		return len(p), io.ErrShortWrite
	}
	return n, err
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return boundedString(line, 256)
		}
	}
	return ""
}

func cloneInt(in *int) *int {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}
func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// decodeLine is shared by the native parsers. Non-JSON lines are represented
// as a bounded diagnostic only; they are never stored as a transcript.
func decodeLine(line []byte) (map[string]any, bool) {
	var value map[string]any
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&value); err != nil || value == nil {
		return nil, false
	}
	return value, true
}
