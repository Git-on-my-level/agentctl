package adapter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type GenericProcessOptions struct {
	OutputLimit int
}

// NewGenericProcess observes any argv command. Probe requires an explicit
// executable path in ProbeRequest; it never guesses a provider or reads a
// shell command string.
func NewGenericProcess(options ...GenericProcessOptions) Adapter {
	config := nativeConfig{
		Manifest:   genericManifest(),
		Parser:     genericParser{},
		LaunchKind: "process",
	}
	if len(options) != 0 {
		config.OutputLimit = options[0].OutputLimit
	}
	return newNativeAdapter(config)
}

func NewCodex() Adapter {
	return newNativeAdapter(nativeConfig{Manifest: codexManifest(), Binary: "codex", Parser: codexParser{}, LaunchKind: "codex_thread"})
}
func NewCursor() Adapter {
	return newNativeAdapter(nativeConfig{Manifest: cursorManifest(), Binary: "cursor-agent", Parser: cursorParser{}, LaunchKind: "cursor_session"})
}
func NewClaudeCode() Adapter {
	return newNativeAdapter(nativeConfig{Manifest: claudeManifest(), Binary: "claude", Parser: claudeParser{}, LaunchKind: "claude_session"})
}

// NewClaude is kept as a concise alias for callers that use the provider name.
func NewClaude() Adapter { return NewClaudeCode() }
func NewOMP() Adapter {
	return newNativeAdapter(nativeConfig{Manifest: ompManifest(), Binary: "omp", Parser: ompParser{}, PollStatus: true, LaunchKind: "omp_session"})
}

type MulticaConfig struct {
	Binary         string
	Profile        string
	Endpoint       string
	Workspace      string
	Issue          string
	Run            string
	EventPageLimit int
}

// MulticaDefaultProfile is an explicit request to use Multica's root/default
// profile. It is distinct from an empty profile (invalid) and never copies or
// discovers credentials into an agentctl profile.
const MulticaDefaultProfile = "@default"

// MulticaArgv constructs one of the currently verified read-only Multica event
// commands. The fork's grammar uses global --profile/--workspace-id flags and
// nested `event list|watch` commands. Unverified issue/run operations return
// nil instead of inventing a command that could mutate authority state.
func MulticaArgv(config MulticaConfig, operation string, extra ...string) []string {
	binary := config.Binary
	if binary == "" {
		binary = "multica"
	}
	var command []string
	switch operation {
	case "event list":
		command = []string{"event", "list"}
	case "event watch":
		command = []string{"event", "watch"}
	default:
		return nil
	}
	argv := []string{binary}
	if config.Profile != MulticaDefaultProfile {
		argv = append(argv, "--profile", config.Profile)
	}
	argv = append(argv, "--workspace-id", config.Workspace)
	if config.Endpoint != "" {
		argv = append(argv, "--server-url", config.Endpoint)
	}
	argv = append(argv, command...)
	return append(argv, extra...)
}

func NewMultica(config MulticaConfig) Adapter {
	if config.Binary == "" {
		config.Binary = "multica"
	}
	base := newNativeAdapter(nativeConfig{
		Manifest: multicaManifest(), Binary: config.Binary, Parser: multicaPageParser{},
		LaunchKind: "multica_event", WholeStdout: true,
	})
	return &multicaAdapter{NativeAdapter: base, config: config}
}

// multicaAdapter exposes only the verified workspace event observer. Issue/run
// lifecycle commands remain capability_unavailable until their current CLI
// grammar and authority semantics are explicitly mapped.
type multicaAdapter struct {
	*NativeAdapter
	config MulticaConfig
}

func (m *multicaAdapter) Name() string       { return m.NativeAdapter.Name() }
func (m *multicaAdapter) Manifest() Manifest { return m.NativeAdapter.Manifest() }

func (m *multicaAdapter) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	req.Profile = m.config.Profile
	req.Endpoint = m.config.Endpoint
	req.Workspace = m.config.Workspace
	result, err := m.NativeAdapter.Probe(ctx, req)
	if err != nil {
		return ProbeResult{}, err
	}
	probe := &multicaAdapter{NativeAdapter: m.NativeAdapter, config: m.config}
	probe.config.EventPageLimit = 1
	if _, err := probe.EventsPage(ctx, EventsRequest{Ref: SourceRef{Adapter: "multica", Kind: "multica_event", Profile: m.config.Profile, Workspace: m.config.Workspace}, Cursor: "0"}); err != nil {
		return ProbeResult{}, fmt.Errorf("verify Multica workspace event capability: %w", err)
	}
	for i := range result.Capabilities {
		if result.Capabilities[i].Name == CapabilityEvents {
			result.Capabilities[i].Status = CapabilitySupported
			result.Capabilities[i].Source = "live_probe"
		}
	}
	return result, nil
}

func (m *multicaAdapter) Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
	return LaunchResult{}, capabilityError(CapabilityLaunch, "Multica launch/run route is not verified for the current CLI")
}

func (m *multicaAdapter) Attach(ctx context.Context, req AttachRequest) (Attachment, error) {
	return Attachment{}, capabilityError(CapabilityAttach, "Multica issue/run attachment route is not verified for the current CLI")
}
func (m *multicaAdapter) Snapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error) {
	return Snapshot{}, capabilityError(CapabilitySnapshot, "Multica issue/run status route is not verified for the current CLI")
}
func (m *multicaAdapter) Events(ctx context.Context, req EventsRequest) ([]Event, error) {
	page, err := m.EventsPage(ctx, req)
	if err != nil {
		return nil, err
	}
	requestCursor := req.Cursor
	if requestCursor == "" {
		requestCursor = "0"
	}
	if page.NextCursor != "" && page.NextCursor != requestCursor {
		// Preserve authority cursor advancement without exposing unrelated
		// workspace events. The checkpoint is metadata-only and is emitted even
		// when this page also contains matching events, so a caller can advance
		// past an unmatched tail without replaying it on the next poll.
		sequence := int64(len(page.Events) + 1)
		checkpoint := Event{Sequence: sequence, Cursor: page.NextCursor, Kind: "health", ObservedAt: time.Now().UTC(), Ordering: "observation", Payload: map[string]any{
			"cursor_advanced": true,
			"filtered":        page.Filtered,
			"scanned":         page.Scanned,
			"page_checkpoint": true,
		}}
		checkpoint.DedupeKey = Fingerprint(m.config.Workspace, "multica_cursor", page.NextCursor)
		page.Events = append(page.Events, checkpoint)
	}
	return page.Events, nil
}

// EventsPage polls one bounded workspace event-list page. It never invokes
// event watch: a daemonless adapter call must have a finite child lifetime.
func (m *multicaAdapter) EventsPage(ctx context.Context, req EventsRequest) (MulticaEventPage, error) {
	if m.config.Profile == "" || m.config.Workspace == "" {
		return MulticaEventPage{}, invalidRequest("Multica event observation requires exact profile and workspace-id")
	}
	if req.Ref.Profile != "" && req.Ref.Profile != m.config.Profile {
		return MulticaEventPage{}, invalidRequest("Multica event profile does not match configured authority")
	}
	if req.Ref.Workspace != "" && req.Ref.Workspace != m.config.Workspace {
		return MulticaEventPage{}, invalidRequest("Multica event workspace does not match configured authority")
	}
	if req.Ref.Issue != "" && req.Ref.Issue != m.config.Issue {
		return MulticaEventPage{}, invalidRequest("Multica event issue does not match configured binding")
	}
	if req.Ref.Run != "" && req.Ref.Run != m.config.Run {
		return MulticaEventPage{}, invalidRequest("Multica event run does not match configured binding")
	}
	if req.Ref.Empty() {
		req.Ref = SourceRef{Adapter: "multica", Kind: "multica_event", Profile: m.config.Profile, Workspace: m.config.Workspace}
	}
	if req.Ref.Kind == "" {
		req.Ref.Kind = "multica_event"
	}
	limit := m.config.EventPageLimit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	cursor := req.Cursor
	if cursor == "" {
		cursor = "0"
	}
	argv := MulticaArgv(m.config, "event list", "--cursor", cursor, "--limit", strconv.Itoa(limit))
	if argv == nil {
		return MulticaEventPage{}, capabilityError(CapabilityEvents, "Multica event command grammar is unavailable")
	}
	launch, err := m.NativeAdapter.runOneShot(ctx, argv, req.Ref)
	if err != nil {
		return MulticaEventPage{}, err
	}
	record := m.NativeAdapter.recordFor(launch.Session.Ref)
	if record == nil {
		return MulticaEventPage{}, &AdapterError{Code: ErrExecutionUnknown, Message: "Multica event page process was not retained", Retryable: true}
	}
	record.mu.Lock()
	pageData := record.page
	events := append([]Event(nil), record.events...)
	record.mu.Unlock()
	if pageData == nil {
		if launch.Result != nil && launch.Result.Error != "" {
			return MulticaEventPage{}, &AdapterError{Code: ErrExecutionFailed, Message: "Multica event list did not return a structured page", Details: map[string]any{"reason": launch.Result.Error}}
		}
		return MulticaEventPage{}, &AdapterError{Code: ErrExecutionUnknown, Message: "Multica event list returned no structured page", Retryable: true}
	}
	page := MulticaEventPage{Events: make([]Event, 0, len(events)), PageLimit: limit}
	page.NextCursor = pageData.NextCursor
	page.Scanned = pageData.Scanned
	for _, event := range events {
		if multicaEventMatches(event, m.config) {
			page.Events = append(page.Events, constrainMulticaAuthority(event, m.config))
		} else {
			page.Filtered++
		}
	}
	if page.Scanned == 0 {
		page.Scanned = len(events)
	}
	return page, nil
}

func constrainMulticaAuthority(event Event, config MulticaConfig) Event {
	if config.Run != "" || event.Kind != "terminal" {
		return event
	}
	aggregate := strings.ToLower(strings.TrimSpace(fmt.Sprint(event.Payload["aggregate_kind"])))
	if aggregate == "" {
		aggregate = strings.ToLower(strings.TrimSpace(fmt.Sprint(event.Payload["aggregate_type"])))
	}
	if aggregate == "issue" {
		return event
	}
	// An issue-only promotion has no authoritative run binding. Nested task or
	// run terminal events are useful evidence, but Multica may retry, rerun, or
	// execute several tasks under one issue. Only the issue aggregate may close
	// the agentctl lifecycle until an exact run is durably bound.
	if event.State == StateFailed || event.State == StateCancelled || event.State == StateOrphaned {
		event.Kind = "attention"
	} else {
		event.Kind = "progress"
	}
	event.State = StateWaiting
	return event
}

func multicaEventMatches(event Event, config MulticaConfig) bool {
	aggregateID, ok := event.Payload["aggregate_id"]
	aggregate := strings.ToLower(strings.TrimSpace(fmt.Sprint(event.Payload["aggregate_kind"])))
	if aggregate == "" {
		aggregate = strings.ToLower(strings.TrimSpace(fmt.Sprint(event.Payload["aggregate_type"])))
	}
	issueMatches := config.Issue != "" && scalarEquals(event.Payload, "issue_id", config.Issue)
	runMatches := config.Run != "" && scalarMatchesAny(event.Payload, config.Run, "run_id", "task_id", "execution_id", "entity_id")
	switch aggregate {
	case "issue":
		// An explicit issue aggregate is authoritative. Do not let a nested
		// selector on an event for another issue promote it into this binding.
		if ok {
			return config.Issue != "" && fmt.Sprint(aggregateID) == config.Issue
		}
		return issueMatches
	case "run", "execution":
		// Run/execution aggregates are authoritative when present. A nested
		// issue selector is useful only for an unscoped event lacking that id.
		if ok {
			return config.Run != "" && fmt.Sprint(aggregateID) == config.Run
		}
		return runMatches || issueMatches
	case "task", "task_run", "agent_task":
		// Task aggregate ids are not necessarily run ids; correlate these by
		// the bounded nested run/issue selectors when available.
		return runMatches || issueMatches || (ok && config.Run != "" && fmt.Sprint(aggregateID) == config.Run)
	default:
		// Events without a recognized aggregate kind are correlated only by
		// explicit nested selectors or an exact aggregate id. A known kind is
		// handled above so an issue cannot match a run via a coincidental field.
		return runMatches || issueMatches || (ok && ((config.Run != "" && fmt.Sprint(aggregateID) == config.Run) || (config.Issue != "" && fmt.Sprint(aggregateID) == config.Issue)))
	}
}

func scalarMatchesAny(payload map[string]any, expected string, keys ...string) bool {
	for _, key := range keys {
		if scalarEquals(payload, key, expected) {
			return true
		}
	}
	return false
}

func scalarEquals(payload map[string]any, key, expected string) bool {
	value, ok := payload[key]
	return ok && expected != "" && fmt.Sprint(value) == expected
}
func (m *multicaAdapter) Result(ctx context.Context, req ResultRequest) (Result, error) {
	return Result{}, capabilityError(CapabilityResult, "Multica issue/run result route is not verified for the current CLI")
}
func (m *multicaAdapter) Resume(ctx context.Context, req ResumeRequest) (LaunchResult, error) {
	return LaunchResult{}, capabilityError(CapabilityResume, "Multica issue/run resume route is not verified for the current CLI")
}
func (m *multicaAdapter) Cancel(ctx context.Context, req CancelRequest) error {
	return capabilityError(CapabilityCancel, "Multica issue/run cancellation route is not verified for the current CLI")
}

func multicaScopedArgv(config MulticaConfig, operation string, ref SourceRef, extra ...string) []string {
	return MulticaArgv(config, operation, extra...)
}

func genericManifest() Manifest {
	return baseManifest("generic-process", "0.1.0", "process", "generic-process-json", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilitySupported), sameProcessDecl(CapabilityAttach, CapabilitySupported), sameProcessDecl(CapabilitySnapshot, CapabilitySupported),
		sameProcessDecl(CapabilityEvents, CapabilityDegraded), sameProcessDecl(CapabilityResult, CapabilityDegraded), capDecl(CapabilityResume, CapabilityUnavailable), sameProcessDecl(CapabilityCancel, CapabilitySupported),
		resultContentDecl(CapabilityDegraded, "generic_result"),
		capDecl(CapabilityContextInjection, CapabilityDegraded),
	})
}

func codexManifest() Manifest {
	resultContent := resultContentDecl(CapabilitySupported, "agent_message")
	resultContent.Constraints["required_output_mode"] = "json"
	resultContent.Constraints["required_argv"] = map[string]any{"flag": "--json", "kind": "presence"}
	return baseManifest("codex", "0.1.0", "codex_thread", "codex-json", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilitySupported), sameProcessDecl(CapabilityAttach, CapabilityDegraded), sameProcessDecl(CapabilitySnapshot, CapabilityDegraded), sameProcessDecl(CapabilityEvents, CapabilitySupported), sameProcessDecl(CapabilityResult, CapabilitySupported), resultContent, capDecl(CapabilityResume, CapabilityUnavailable), sameProcessDecl(CapabilityCancel, CapabilitySupported), capDecl(CapabilityContextInjection, CapabilityDegraded),
	})
}

func cursorManifest() Manifest {
	resultContent := resultContentDecl(CapabilitySupported, "result_or_assistant_message")
	resultContent.Constraints["required_output_mode"] = "stream-json"
	resultContent.Constraints["required_argv"] = map[string]any{"flag": "--output-format", "kind": "value", "value": "stream-json"}
	return baseManifest("cursor", "0.1.0", "cursor_session", "cursor-stream-json", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilitySupported), sameProcessDecl(CapabilityAttach, CapabilityDegraded), sameProcessDecl(CapabilitySnapshot, CapabilityDegraded), sameProcessDecl(CapabilityEvents, CapabilitySupported), sameProcessDecl(CapabilityResult, CapabilitySupported), resultContent, capDecl(CapabilityResume, CapabilityUnavailable), sameProcessDecl(CapabilityCancel, CapabilitySupported), capDecl(CapabilityContextInjection, CapabilityDegraded),
	})
}

func claudeManifest() Manifest {
	resultContent := resultContentDecl(CapabilitySupported, "result")
	resultContent.Constraints["required_output_mode"] = "stream-json"
	resultContent.Constraints["required_argv"] = map[string]any{"flag": "--output-format", "kind": "value", "value": "stream-json"}
	return baseManifest("claude-code", "0.1.0", "claude_session", "claude-stream-json", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilitySupported), sameProcessDecl(CapabilityAttach, CapabilityDegraded), sameProcessDecl(CapabilitySnapshot, CapabilityDegraded), sameProcessDecl(CapabilityEvents, CapabilitySupported), sameProcessDecl(CapabilityResult, CapabilitySupported), resultContent, capDecl(CapabilityResume, CapabilityUnavailable), sameProcessDecl(CapabilityCancel, CapabilitySupported), capDecl(CapabilityContextInjection, CapabilityDegraded),
	})
}

func ompManifest() Manifest {
	resultContent := resultContentDecl(CapabilitySupported, "agent_end_or_assistant_message")
	resultContent.Constraints["required_output_mode"] = "json"
	resultContent.Constraints["required_argv"] = map[string]any{"flag": "--mode", "kind": "value", "value": "json"}
	m := baseManifest("omp", "0.2.0", "omp_session", "omp-acp-json", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilitySupported), sameProcessDecl(CapabilityAttach, CapabilityDegraded), sameProcessDecl(CapabilitySnapshot, CapabilityDegraded), sameProcessDecl(CapabilityEvents, CapabilityDegraded), sameProcessDecl(CapabilityResult, CapabilitySupported), resultContent, capDecl(CapabilityResume, CapabilityUnavailable), sameProcessDecl(CapabilityCancel, CapabilitySupported), capDecl(CapabilityContextInjection, CapabilityDegraded),
	})
	m.KnownFailures = []KnownFailure{{Classifier: "stats_probe", ErrorCode: "local_operational_write"}}
	return m
}

func multicaManifest() Manifest {
	events := capDecl(CapabilityEvents, CapabilityDegraded)
	events.Constraints = map[string]any{"scope": "workspace_events", "cross_restart": true, "source": "native_cli"}
	m := baseManifest("multica", "0.1.0", "multica_run", "multica-json", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilityUnavailable), capDecl(CapabilityAttach, CapabilityUnavailable), capDecl(CapabilitySnapshot, CapabilityUnavailable), events, capDecl(CapabilityResult, CapabilityUnavailable), capDecl(CapabilityResultContent, CapabilityUnavailable), capDecl(CapabilityResume, CapabilityUnavailable), capDecl(CapabilityCancel, CapabilityUnavailable), capDecl(CapabilityContextInjection, CapabilityDegraded),
	})
	m.ContextInjection = ContextInjection{Mechanisms: []ContextMechanism{ContextEnvironmentPath, ContextAuthorityArtifact}, Guaranteed: false, Reason: "delivery must be explicitly verified by the Multica worker"}
	return m
}

func baseManifest(name, version, kind, format string, capabilities []CapabilityDeclaration) Manifest {
	executable := kind
	switch name {
	case "codex":
		executable = "codex"
	case "cursor":
		executable = "cursor-agent"
	case "claude-code":
		executable = "claude"
	case "omp":
		executable = "omp"
	case "multica":
		executable = "multica"
	case "generic-process":
		executable = "configured"
	}
	probeActions := []ProbeAction{}
	if name != "generic-process" {
		probeActions = append(probeActions, ProbeAction{ID: "version", Argv: []string{"{executable}", "--version"}, SideEffectClass: "read_only", WritesCache: false, TimeoutSeconds: 5})
	}
	return Manifest{SchemaVersion: ManifestSchemaVersion, Adapter: name, AdapterVersion: version, CoreCompatibility: VersionRange{Minimum: "1.0.0", MaximumExclusive: "2.0.0"}, DocumentSchemaVersions: map[string][]int{"execution": {1}, "event": {1}}, ExecutableDiscovery: []ExecutableRule{{Kind: "path_lookup", Value: executable, Required: true}}, BackendCompatibility: []BackendCompatibility{}, Capabilities: capabilities, ProbeActions: probeActions, StructuredOutput: StructuredOutputSpec{Format: format, Required: true, DiscoveryKeys: []string{"session_id", "thread_id", "run_id"}, TerminalKeys: []string{"result", "completed", "failed"}}, ContextInjection: ContextInjection{Mechanisms: []ContextMechanism{ContextEnvironmentPath}, Guaranteed: false, Reason: "handle is provided to the child but model delivery is not guaranteed"}, Sensitivity: map[string]string{"opaque_id": "operator-private", "artifact": "project-confidential"}}
}

func capDecl(name CapabilityName, status CapabilityStatus) CapabilityDeclaration {
	if status == CapabilityDegraded {
		status = CapabilityConditional
	}
	return CapabilityDeclaration{Name: name, SemanticsVersion: SemanticsVersion, Implementation: status, Constraints: map[string]any{}}
}

func sameProcessDecl(name CapabilityName, status CapabilityStatus) CapabilityDeclaration {
	decl := capDecl(name, status)
	decl.Constraints = map[string]any{"scope": "same_process_only", "cross_restart": false}
	return decl
}

func resultContentDecl(status CapabilityStatus, source string) CapabilityDeclaration {
	decl := sameProcessDecl(CapabilityResultContent, status)
	decl.Constraints["source"] = source
	decl.Constraints["max_bytes"] = 1 << 20
	return decl
}

// NegotiateInvocation resolves one static adapter declaration against the
// exact native argv. It performs no filesystem or network access and is safe
// to use before dependency probing or journal creation.
func NegotiateInvocation(manifest Manifest, argv []string, name CapabilityName) Capability {
	for _, declaration := range manifest.Capabilities {
		if declaration.Name != name {
			continue
		}
		status := declaration.Implementation
		if status == CapabilityConditional {
			status = CapabilityDegraded
		}
		if status == "" {
			status = CapabilitySupported
		}
		constraints := cloneMap(declaration.Constraints)
		reason := ""
		if len(argv) != 0 && name == CapabilityResultContent && !invocationRequirementSatisfied(argv, constraints) {
			status = CapabilityUnavailable
			constraints["invocation_satisfied"] = false
			reason = "exact invocation does not satisfy the structured-output requirement"
		} else if len(argv) != 0 && name == CapabilityResultContent {
			constraints["invocation_satisfied"] = true
		}
		return Capability{Name: name, Status: status, Source: "manifest", SemanticsVersion: declaration.SemanticsVersion, Constraints: constraints, Reason: reason}
	}
	return Capability{Name: name, Status: CapabilityUnavailable, Source: "manifest", SemanticsVersion: SemanticsVersion, Constraints: map[string]any{}, Reason: "capability is not declared"}
}
