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
	maxStructuredRecordBytes   = 8 << 20
	nativeObservationFreshness = 30 * time.Second
)

const (
	diagnosticStreamLimitExceeded  = "structured_stream_limit_exceeded"
	diagnosticRecordTooLarge       = "structured_record_too_large"
	diagnosticResultFileUnreadable = "result_file_unreadable"
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
	metadataSeen     map[string]bool
	metadataState    State
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
	overLimit := p.stdoutBytes+p.stderrBytes > p.maxOutput
	if overLimit {
		p.addParseWarningLocked(diagnosticStreamLimitExceeded)
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
	if overLimit && !retainObservationAfterLimit(obs) {
		return
	}
	p.ingestObservation(obs)
}

func retainObservationAfterLimit(obs parsedObservation) bool {
	return obs.Terminal || obs.Content != "" || obs.Error != "" || obs.State == StateAttention || obs.State == StateWaiting || obs.Kind == "attention"
}

func (p *processRecord) addParseWarningLocked(code string) {
	for _, existing := range p.parseWarnings {
		if existing == code {
			return
		}
	}
	p.parseWarnings = append(p.parseWarnings, code)
}

func (p *processRecord) addParseWarning(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.addParseWarningLocked(code)
}

func applyParseWarning(result *Result, warnings []string) {
	if result == nil || len(warnings) == 0 {
		return
	}
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	if _, exists := result.Data["diagnostic_code"]; !exists {
		result.Data["diagnostic_code"] = warnings[0]
	} else {
		result.Data["parse_diagnostic_code"] = warnings[0]
	}
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
	if obs.State != "" && p.metadataState != "" && obs.State != p.metadataState {
		// The same metadata signature after a real state transition is new
		// evidence, so start a fresh coalescing window.
		p.metadataSeen = nil
	}
	if obs.State != "" {
		p.metadataState = obs.State
	}
	if key, eligible := metadataObservationKey(obs); eligible {
		if p.metadataSeen == nil {
			p.metadataSeen = map[string]bool{}
		}
		if p.metadataSeen[key] {
			// Native token and thinking streams can emit many thousands of observations
			// with the same bounded metadata. Retain one semantic event for each
			// distinct phase instead of turning the journal into a transcript-shaped
			// counter. Terminal, attention, artifact, state changes, and authority-
			// positioned events are deliberately ineligible for this coalescing.
			p.updatedAt = time.Now().UTC()
			return
		}
		p.metadataSeen[key] = true
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
		if data == nil {
			data = map[string]any{}
		}
		if obs.SourceState != "" {
			data["terminal_source_state"] = obs.SourceState
		}
		result := &Result{Success: obs.Success, State: obs.State, Summary: firstNonEmpty(obs.Summary, boundedString(content, 2048)), Content: content, ContentType: contentType, ContentTruncated: obs.ContentTruncated || p.contentTruncated, Error: obs.Error, SessionRef: p.ref, Data: data}
		applyParseWarning(result, p.parseWarnings)
		p.result = result
	}
	p.updatedAt = time.Now().UTC()
}

func metadataObservationKey(obs parsedObservation) (string, bool) {
	if (obs.Kind != "health" && obs.Kind != "progress") || obs.Terminal || obs.Content != "" || obs.Error != "" || obs.CursorAuthority || obs.SourcePosition != "" {
		return "", false
	}
	key := strings.Join([]string{obs.Kind, obs.SourceState, string(obs.State), string(obs.Liveness), string(marshalStable(obs.Data))}, "\x00")
	return key, true
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
				if data == nil {
					data = map[string]any{}
				}
				if content != "" {
					data["result_content_source"] = firstNonEmpty(obs.ContentSource, p.contentSource, "terminal_result")
				}
				if obs.SourceState != "" {
					data["terminal_source_state"] = obs.SourceState
				}
				p.result = &Result{Success: obs.Success, State: obs.State, Summary: obs.Summary, Content: content, ContentType: firstNonEmpty(obs.ContentType, p.contentType), ContentTruncated: obs.ContentTruncated || p.contentTruncated, Error: obs.Error, ExitCode: p.exitCode, SessionRef: p.ref, Data: data}
			}
		} else if readErr != nil && !os.IsNotExist(readErr) {
			p.addParseWarningLocked(diagnosticResultFileUnreadable)
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
			p.result = &Result{Success: false, State: StateOrphaned, Error: "native process exited cleanly without an explicit structured terminal result", ExitCode: p.exitCode, SessionRef: p.ref, Data: map[string]any{"diagnostic_code": "result_extraction_failed"}}
		}
	}
	applyParseWarning(p.result, p.parseWarnings)
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
	// HardOneShotDeadline makes finite probe commands kill their process group
	// and return as soon as the caller context expires. The background waiter
	// still owns reaping; this avoids extending a read deadline with the normal
	// five-second graceful-cancellation window.
	HardOneShotDeadline bool
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
		capabilities = append(capabilities, NegotiateInvocation(a.config.Manifest, req.Argv, declaration.Name))
	}
	invocationFingerprint := ""
	if len(req.Argv) != 0 {
		invocationFingerprint = Fingerprint(req.Argv...)
	}
	scope := Fingerprint(a.Name(), backendVersion, digest, req.Profile, req.Endpoint, req.Workspace, invocationFingerprint)
	return ProbeResult{AdapterVersion: a.config.Manifest.AdapterVersion, BackendVersion: backendVersion, Executable: resolved, ExecutableDigest: digest, ScopeFingerprint: scope, ProbedAt: started, FreshFor: time.Minute, ReadOnly: true, Capabilities: capabilities}, nil
}

func invocationRequirementSatisfied(argv []string, constraints map[string]any) bool {
	requirement, ok := constraints["required_argv"].(map[string]any)
	if !ok || len(requirement) == 0 {
		return true
	}
	flag, _ := requirement["flag"].(string)
	kind, _ := requirement["kind"].(string)
	want, _ := requirement["value"].(string)
	matchedPresence := false
	seenValue := false
	actualValue := ""
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			break
		}
		if kind == "presence" && arg == flag {
			matchedPresence = true
			continue
		}
		if kind != "value" {
			continue
		}
		if arg == flag {
			seenValue = true
			actualValue = ""
			if i+1 < len(argv) && argv[i+1] != "--" {
				actualValue = argv[i+1]
				i++
			}
			continue
		}
		if value, found := strings.CutPrefix(arg, flag+"="); found {
			seenValue = true
			actualValue = value
		}
	}
	if kind == "presence" {
		return matchedPresence
	}
	return seenValue && strings.EqualFold(actualValue, want)
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
	if err := runCtx.Err(); err != nil {
		return LaunchResult{}, nativeContextError(runCtx, "native process launch")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = append(os.Environ(), req.Env...)
	if req.Stdin != nil {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
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
	if req.StartOnly {
		// exec.Command deliberately leaves signal escalation under adapter
		// control. Preserve the caller/deadline ownership that CommandContext
		// previously supplied, while using the same TERM-then-KILL policy as an
		// explicit cancel request.
		ownedCancel := timeoutCancel
		timeoutCancel = nil
		go func() {
			if ownedCancel != nil {
				defer ownedCancel()
			}
			select {
			case <-runCtx.Done():
				a.cancelRecord(record, "term", 5*time.Second)
			case <-record.done:
			}
		}()
	}
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
		if _, err := a.waitRecord(runCtx, record, true, req.hardContextDeadline); err != nil {
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
			record.addParseWarning(diagnosticRecordTooLarge)
			return
		}
		if len(document) > limit {
			record.addParseWarning(diagnosticStreamLimitExceeded)
			return
		}
		record.ingest(document, false)
		return
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxStructuredRecordBytes)
	for scanner.Scan() {
		record.ingest(scanner.Bytes(), stderr)
	}
	if err := scanner.Err(); err != nil {
		record.addParseWarning(diagnosticRecordTooLarge)
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
	if _, err := a.waitRecord(ctx, record, true, false); err != nil {
		return Result{}, err
	}
	result := a.currentResult(record)
	if result == nil {
		return Result{}, &AdapterError{Code: ErrExecutionUnknown, Message: "native process ended without a result", Retryable: true}
	}
	return *result, nil
}

func (a *NativeAdapter) waitRecord(ctx context.Context, record *processRecord, killOnCancel, hardContextDeadline bool) (*Result, error) {
	select {
	case <-record.done:
		return a.currentResult(record), nil
	case <-ctx.Done():
		if killOnCancel {
			if hardContextDeadline {
				a.cancelRecord(record, "kill", 0)
			} else {
				a.cancelRecord(record, "term", 5*time.Second)
			}
		}
		// A hard finite-probe deadline returns after killing the process group;
		// the existing waiter continues to reap it. Normal native work preserves
		// the graceful cancel-and-reap contract.
		if !hardContextDeadline {
			<-record.done
		}
		code, message := ErrExecutionCancelled, "native process cancelled while waiting"
		if ctx.Err() == context.DeadlineExceeded {
			code, message = ErrTimeout, "native process timed out while waiting"
		}
		return a.currentResult(record), &AdapterError{Code: code, Message: message, Retryable: code == ErrTimeout, Cause: ctx.Err()}
	}
}

func (a *NativeAdapter) cancelRecord(record *processRecord, signal string, grace time.Duration) {
	record.mu.Lock()
	record.cancelled = true
	cmd := record.cmd
	record.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		if signal == "kill" || grace <= 0 {
			_ = killProcess(cmd)
			return
		}
		_ = terminateProcess(cmd)
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-record.done:
			return
		case <-timer.C:
			_ = killProcess(cmd)
		}
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
		record.mu.Lock()
		defer record.mu.Unlock()
		if record.result != nil {
			result := *record.result
			result.SessionRef = record.ref
			result.Data = cloneMap(record.result.Data)
			return result, nil
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
		a.cancelRecord(record, req.Signal, req.Grace)
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
	return a.Launch(ctx, LaunchRequest{Argv: argv, DiscoveryWindow: 250 * time.Millisecond, StartOnly: false, hardContextDeadline: a.config.HardOneShotDeadline})
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
