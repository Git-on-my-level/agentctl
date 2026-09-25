package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// NewZCode launches the ZCode CLI. Headless runs print one JSON document, not
// JSONL. The model and effort live in the CLI config, not argv.
func NewZCode() Adapter {
	bin := resolveZCodeBinary()
	base := newNativeAdapter(nativeConfig{
		Manifest: zcodeManifest(), Binary: bin, Parser: zcodeParser{},
		LaunchKind: "zcode_session", WholeStdout: true, TransformArgv: rewriteZCodeArgv,
	})
	return &zcodeAdapter{NativeAdapter: base}
}

// NewDevin launches the Devin CLI. Print mode (`-p`) writes one plain-text
// answer, optionally after a welcome banner. It is not JSON. `devin acp` is a
// separate newline JSON-RPC server and is not the print payload.
func NewDevin() Adapter {
	bin := resolveDevinBinary()
	base := newNativeAdapter(nativeConfig{
		Manifest: devinManifest(), Binary: bin, Parser: devinParser{},
		LaunchKind: "devin_session", WholeStdout: true, TransformArgv: rewriteDevinArgv,
	})
	return &devinAdapter{NativeAdapter: base}
}

// NewOpenClaw runs an embedded local agent turn. The gateway-backed ACP bridge
// is deliberately outside this adapter's authority.
func NewOpenClaw() Adapter {
	base := newNativeAdapter(nativeConfig{
		Manifest: openclawManifest(), Binary: resolveOpenClawBinary(), Parser: openclawParser{},
		LaunchKind: "openclaw_session", WholeStdout: true, TransformArgv: rewriteOpenClawArgv,
	})
	return &openclawAdapter{NativeAdapter: base}
}

type openclawAdapter struct{ *NativeAdapter }

func (o *openclawAdapter) Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
	if !openclawInvocationSatisfied(req.Argv) {
		return LaunchResult{}, invalidRequest("openclaw adapter requires agent --local --json")
	}
	return o.NativeAdapter.Launch(ctx, req)
}

func argvHasFlag(argv []string, flag string) bool {
	for _, arg := range argv[1:] {
		if arg == "--" {
			break
		}
		if arg == flag {
			return true
		}
	}
	return false
}

func argvHasOption(argv []string, option string) bool {
	for _, arg := range argv[1:] {
		if arg == "--" {
			break
		}
		if arg == option || strings.HasPrefix(arg, option+"=") {
			return true
		}
	}
	return false
}

func openclawInvocationSatisfied(argv []string) bool {
	return len(argv) > 1 && argv[1] == "agent" && argvHasFlag(argv, "--local") && argvHasFlag(argv, "--json")
}

func (o *openclawAdapter) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	if req.Executable == "" || req.Executable == "openclaw" {
		req.Executable = resolveOpenClawBinary()
	}
	return o.NativeAdapter.Probe(ctx, req)
}

func resolveOpenClawBinary() string {
	if path, err := exec.LookPath("openclaw"); err == nil {
		return path
	}
	return "openclaw"
}

func rewriteOpenClawArgv(argv []string) []string {
	if len(argv) == 0 {
		return argv
	}
	out := append([]string(nil), argv...)
	if out[0] == "openclaw" {
		out[0] = resolveOpenClawBinary()
	}
	if len(out) > 1 && out[1] == "agent" && !argvHasOption(out, "--session-key") && !argvHasOption(out, "--session-id") && !argvHasOption(out, "--to") && !argvHasOption(out, "-t") {
		// A new local session for each invocation avoids accidental transcript
		// reuse through OpenClaw's default agent session.
		agentID := "main"
		for i := 2; i < len(out); i++ {
			if out[i] == "--agent" && i+1 < len(out) {
				agentID = out[i+1]
			} else if value, ok := strings.CutPrefix(out[i], "--agent="); ok {
				agentID = value
			}
		}
		key := "agent:" + agentID + ":agentctl-" + rand.Text()
		out = append([]string{out[0], out[1], "--session-key", key}, out[2:]...)
	}
	return out
}

type devinAdapter struct{ *NativeAdapter }

func (d *devinAdapter) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	if req.Executable == "" || req.Executable == "devin" {
		req.Executable = resolveDevinBinary()
	}
	return d.NativeAdapter.Probe(ctx, req)
}

func resolveDevinBinary() string {
	if path, err := exec.LookPath("devin"); err == nil {
		return path
	}
	return "devin"
}

func rewriteDevinArgv(argv []string) []string {
	if len(argv) == 0 || argv[0] != "devin" {
		return argv
	}
	resolved := resolveDevinBinary()
	if resolved == "" || resolved == argv[0] {
		return argv
	}
	out := make([]string, len(argv))
	out[0] = resolved
	copy(out[1:], argv[1:])
	return out
}

type zcodeAdapter struct{ *NativeAdapter }

func (z *zcodeAdapter) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	if req.Executable == "" || req.Executable == "zcode" {
		req.Executable = resolveZCodeBinary()
	}
	result, err := z.NativeAdapter.Probe(ctx, req)
	if err != nil && req.Executable == "zcode" {
		var adapterErr *AdapterError
		if errors.As(err, &adapterErr) && adapterErr.Code == ErrDependencyUnavailable && adapterErr.Message == "native executable is unavailable" {
			return result, zcodeLauncherMissing(err)
		}
	}
	return result, err
}

// zcodeAppBundles lists where ZCode.app may be installed. Current app builds
// bundle the CLI at Contents/Resources/glm/zcode.cjs but install no `zcode`
// command and no standalone node, so a missing launcher is the common case.
var zcodeAppBundles = func() []string {
	bundles := []string{"/Applications/ZCode.app"}
	if home, err := os.UserHomeDir(); err == nil {
		bundles = append(bundles, filepath.Join(home, "Applications", "ZCode.app"))
	}
	return bundles
}

// zcodeLauncherMissing names the fix instead of the generic "native executable
// is unavailable": which paths were searched, and whether an app-bundled CLI
// exists that only needs a PATH launcher.
func zcodeLauncherMissing(cause error) error {
	searched := []string{"PATH: zcode"}
	if home, err := os.UserHomeDir(); err == nil {
		searched = append(searched, filepath.Join(home, ".zcode", "server", "agents", "glm", "zcode-agent"))
	}
	details := map[string]any{
		"diagnostic_code": "zcode_launcher_missing",
		"searched":        searched,
		"remediation":     "install ZCode.app (it bundles the CLI), then put a `zcode` launcher on PATH",
	}
	for _, bundle := range zcodeAppBundles() {
		entry := filepath.Join(bundle, "Contents", "Resources", "glm", "zcode.cjs")
		runtime := filepath.Join(bundle, "Contents", "MacOS", "ZCode")
		if st, err := os.Stat(entry); err != nil || st.IsDir() {
			continue
		}
		details["app_bundled_cli"] = entry
		details["remediation"] = "ZCode.app bundles the CLI but installs no `zcode` command. Put a `zcode` launcher on PATH that runs " +
			"`ELECTRON_RUN_AS_NODE=1 " + runtime + " " + entry + " \"$@\"` with ZCODE_BUILTIN_PROVIDER_CONFIG_FILE=" +
			filepath.Join(bundle, "Contents", "Resources", "config", "provider", "zcode-builtin.json") +
			" and ZCODE_PERSONAL_PROVIDER_CONFIG_FILE set to a CLI-owned provider_config.json that has a default model"
		break
	}
	return &AdapterError{Code: ErrDependencyUnavailable, Message: "zcode launcher is not on PATH", Retryable: true, Details: details, Cause: cause}
}

func resolveZCodeBinary() string {
	if path, err := exec.LookPath("zcode"); err == nil {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "zcode"
	}
	candidate := filepath.Join(home, ".zcode", "server", "agents", "glm", "zcode-agent")
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return candidate
	}
	return "zcode"
}

func rewriteZCodeArgv(argv []string) []string {
	if len(argv) == 0 || argv[0] != "zcode" {
		return argv
	}
	resolved := resolveZCodeBinary()
	if resolved == "" || resolved == argv[0] {
		return argv
	}
	out := make([]string, len(argv))
	out[0] = resolved
	copy(out[1:], argv[1:])
	return out
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

// MulticaArgv constructs verified Multica CLI argv for supported authority
// operations. The fork's grammar uses global --profile/--workspace-id flags and
// nested subcommands. Unverified issue/run operations return nil instead of
// inventing a command that could mutate authority state.
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
	case "issue get":
		command = []string{"issue", "get"}
	case "issue cancel-task":
		command = []string{"issue", "cancel-task"}
	case "workspace get":
		command = []string{"workspace", "get"}
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
		LaunchKind: "multica_event", WholeStdout: true, HardOneShotDeadline: true,
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
	workspace, err := m.readMulticaJSON(ctx, "workspace get", m.config.Workspace, "--output", "json")
	if err != nil {
		return ProbeResult{}, fmt.Errorf("verify Multica workspace authority: %w", err)
	}
	if id, _ := workspace["id"].(string); !strings.EqualFold(strings.TrimSpace(id), strings.TrimSpace(m.config.Workspace)) {
		return ProbeResult{}, fmt.Errorf("verify Multica workspace authority: %w", &AdapterError{Code: ErrExecutionFailed, Message: "Multica workspace get returned a different workspace identity"})
	}
	if m.config.Issue != "" {
		if _, err := m.issueStatus(ctx); err != nil {
			return ProbeResult{}, fmt.Errorf("verify Multica bound issue authority: %w", err)
		}
		for i := range result.Capabilities {
			if result.Capabilities[i].Name == CapabilitySnapshot {
				result.Capabilities[i].Status = CapabilitySupported
				result.Capabilities[i].Source = "live_probe"
			}
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
	if m.config.Profile == "" || m.config.Workspace == "" {
		return Snapshot{}, invalidRequest("Multica issue snapshot requires exact profile and workspace-id")
	}
	if m.config.Issue == "" {
		return Snapshot{}, capabilityError(CapabilitySnapshot, "Multica issue snapshot requires a bound issue")
	}
	if req.Ref.Adapter != "" && req.Ref.Adapter != "multica" {
		return Snapshot{}, invalidRequest("Multica snapshot adapter does not match configured authority")
	}
	if req.Ref.Kind != "" && req.Ref.Kind != "multica_issue" {
		return Snapshot{}, invalidRequest("Multica snapshot source kind does not match the bound issue")
	}
	if req.Ref.OpaqueID != "" && req.Ref.OpaqueID != m.config.Issue {
		return Snapshot{}, invalidRequest("Multica snapshot opaque ID does not match configured binding")
	}
	if req.Ref.Fingerprint != "" && req.Ref.Fingerprint != Fingerprint("multica", "multica_issue", m.config.Issue) {
		return Snapshot{}, invalidRequest("Multica snapshot fingerprint does not match configured binding")
	}
	if req.Ref.Endpoint != "" && req.Ref.Endpoint != m.config.Endpoint {
		return Snapshot{}, invalidRequest("Multica snapshot endpoint does not match configured authority")
	}
	if req.Ref.Profile != "" && req.Ref.Profile != m.config.Profile {
		return Snapshot{}, invalidRequest("Multica snapshot profile does not match configured authority")
	}
	if req.Ref.Workspace != "" && req.Ref.Workspace != m.config.Workspace {
		return Snapshot{}, invalidRequest("Multica snapshot workspace does not match configured authority")
	}
	if req.Ref.Issue != "" && req.Ref.Issue != m.config.Issue {
		return Snapshot{}, invalidRequest("Multica snapshot issue does not match configured binding")
	}
	if req.Ref.Run != "" && req.Ref.Run != m.config.Run {
		return Snapshot{}, invalidRequest("Multica snapshot run does not match configured binding")
	}
	ref := req.Ref
	if ref.Empty() {
		ref = SourceRef{Adapter: "multica", Kind: "multica_issue", OpaqueID: m.config.Issue, Profile: m.config.Profile, Endpoint: m.config.Endpoint, Workspace: m.config.Workspace, Issue: m.config.Issue, Run: m.config.Run}
	}
	if ref.Kind == "" {
		ref.Kind = "multica_issue"
	}
	if ref.OpaqueID == "" {
		ref.OpaqueID = m.config.Issue
	}
	issue, err := m.issueStatus(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	state, liveness := multicaIssueObservation(issue.statusCategory, issue.status)
	now := time.Now().UTC()
	session := Session{Ref: ref, State: state, Liveness: liveness, UpdatedAt: now, Observation: Observation{Source: "status_api", Integrity: "verified", ObservedAt: now}}
	if !issue.updatedAt.IsZero() {
		session.UpdatedAt = issue.updatedAt
	}
	return Snapshot{Session: session}, nil
}

type multicaIssueSnapshot struct {
	id             string
	workspaceID    string
	status         string
	statusCategory string
	updatedAt      time.Time
}

func (m *multicaAdapter) issueStatus(ctx context.Context) (multicaIssueSnapshot, error) {
	document, err := m.readMulticaJSON(ctx, "issue get", m.config.Issue, "--output", "json")
	if err != nil {
		return multicaIssueSnapshot{}, err
	}
	snapshot := multicaIssueSnapshot{
		id:             multicaStringField(document, "id"),
		workspaceID:    multicaStringField(document, "workspace_id"),
		status:         multicaStringField(document, "status"),
		statusCategory: multicaStringField(document, "status_category"),
	}
	if updated, _ := document["updated_at"].(string); updated != "" {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, updated); parseErr == nil {
			snapshot.updatedAt = parsed.UTC()
		}
	}
	if snapshot.id == "" || !strings.EqualFold(snapshot.id, strings.TrimSpace(m.config.Issue)) {
		return multicaIssueSnapshot{}, &AdapterError{Code: ErrExecutionFailed, Message: "Multica issue get returned a different issue identity"}
	}
	if snapshot.workspaceID == "" || !strings.EqualFold(snapshot.workspaceID, strings.TrimSpace(m.config.Workspace)) {
		return multicaIssueSnapshot{}, &AdapterError{Code: ErrExecutionFailed, Message: "Multica issue get returned a different workspace identity"}
	}
	return snapshot, nil
}

func multicaIssueObservation(category, status string) (State, Liveness) {
	key := strings.ToLower(strings.TrimSpace(category))
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(status))
	}
	switch key {
	case "backlog", "todo":
		return StateWaiting, LivenessBlocked
	case "in_progress":
		return StateRunning, LivenessAlive
	case "in_review", "blocked":
		return StateAttention, LivenessBlocked
	case "done":
		return StateCompleted, LivenessExited
	case "cancelled":
		return StateCancelled, LivenessExited
	default:
		return StateAttention, LivenessBlocked
	}
}

func multicaStringField(document map[string]any, key string) string {
	value, _ := document[key].(string)
	return strings.TrimSpace(value)
}

func (m *multicaAdapter) readMulticaJSON(ctx context.Context, operation string, args ...string) (map[string]any, error) {
	argv := MulticaArgv(m.config, operation, args...)
	if argv == nil {
		return nil, capabilityError(CapabilitySnapshot, "Multica "+operation+" route is not verified for the current CLI")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	prepareProcess(cmd)
	cmd.Cancel = func() error { return killProcess(cmd) }
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: 1 << 20}
	cmd.Stderr = &limitedWriter{w: &stderr, n: 64 << 10}
	if err := cmd.Run(); err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		details := map[string]any{"operation": operation, "upstream_exit_code": exitCode}
		if ctx.Err() != nil {
			return nil, &AdapterError{Code: ErrExecutionFailed, Message: "Multica " + operation + " did not complete", Retryable: true, Cause: ctx.Err(), Details: details}
		}
		return nil, &AdapterError{Code: ErrExecutionFailed, Message: "Multica " + operation + " was rejected", Retryable: true, Details: details}
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, &AdapterError{Code: ErrExecutionFailed, Message: "Multica " + operation + " did not return a JSON object", Retryable: true}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, &AdapterError{Code: ErrExecutionFailed, Message: "Multica " + operation + " returned trailing output", Retryable: true}
	}
	return document, nil
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
			return MulticaEventPage{}, &AdapterError{Code: ErrExecutionFailed, Message: "Multica event list did not return a structured page", Retryable: true, Details: map[string]any{"reason": launch.Result.Error}}
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
	if m.config.Profile == "" || m.config.Workspace == "" {
		return invalidRequest("Multica cancel requires exact profile and workspace-id")
	}
	if m.config.Issue == "" || m.config.Run == "" {
		return invalidRequest("Multica cancel requires bound issue and task from the journaled execution")
	}
	argv := MulticaArgv(m.config, "issue cancel-task", m.config.Run, "--issue", m.config.Issue, "--output", "json")
	if len(argv) == 0 {
		return capabilityError(CapabilityCancel, "Multica issue cancel-task route is not verified for the current CLI")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		retryable := ctx.Err() != nil
		details := map[string]any{"stderr": strings.TrimSpace(stderr.String())}
		return &AdapterError{Code: ErrExecutionFailed, Message: "Multica issue cancel-task was rejected", Retryable: retryable, Cause: err, Details: details}
	}
	return nil
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

func devinManifest() Manifest {
	resultContent := resultContentDecl(CapabilitySupported, "print")
	resultContent.Constraints["required_output_mode"] = "print"
	resultContent.Constraints["required_argv"] = map[string]any{"flag": "-p", "kind": "presence"}
	return baseManifest("devin", "0.1.0", "devin_session", "devin-print", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilitySupported), sameProcessDecl(CapabilityAttach, CapabilityDegraded), sameProcessDecl(CapabilitySnapshot, CapabilityDegraded), sameProcessDecl(CapabilityEvents, CapabilityDegraded), sameProcessDecl(CapabilityResult, CapabilitySupported), resultContent, capDecl(CapabilityResume, CapabilityUnavailable), sameProcessDecl(CapabilityCancel, CapabilitySupported), capDecl(CapabilityContextInjection, CapabilityDegraded),
	})
}

func openclawManifest() Manifest {
	resultContent := resultContentDecl(CapabilitySupported, "payloads[].text")
	resultContent.Constraints["required_output_mode"] = "json"
	resultContent.Constraints["required_argv"] = map[string]any{"flag": "--json", "kind": "presence"}
	resultContent.Constraints["transport"] = "embedded_local"
	capabilities := []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilitySupported),
		sameProcessDecl(CapabilityAttach, CapabilityUnavailable),
		sameProcessDecl(CapabilitySnapshot, CapabilityDegraded),
		sameProcessDecl(CapabilityEvents, CapabilityDegraded),
		sameProcessDecl(CapabilityResult, CapabilitySupported),
		resultContent,
		capDecl(CapabilityResume, CapabilityUnavailable),
		sameProcessDecl(CapabilityCancel, CapabilitySupported),
		capDecl(CapabilityContextInjection, CapabilityDegraded),
	}
	capabilities[0].Constraints = map[string]any{"transport": "embedded_local", "required_flags": []string{"agent", "--local", "--json", "-m"}, "session_selector": "--agent, --session-key, --session-id, or --to"}
	capabilities[1].Constraints["reason"] = "no verified attach to a running local CLI process"
	capabilities[2].Constraints["reason"] = "only the agentctl-owned process can be observed"
	capabilities[3].Constraints["reason"] = "only local process observations; no durable native event stream"
	capabilities[6].Constraints = map[string]any{"reason": "CLI session flags exist, but agentctl does not bind verified continuation to a prior run"}
	capabilities[8].Constraints = map[string]any{"reason": "environment context handle is not guaranteed to reach the model"}
	return baseManifest("openclaw", "0.1.0", "openclaw_session", "openclaw-json", capabilities)
}

func zcodeManifest() Manifest {
	resultContent := resultContentDecl(CapabilitySupported, "response")
	resultContent.Constraints["required_output_mode"] = "json"
	resultContent.Constraints["required_argv"] = map[string]any{"flag": "--json", "kind": "presence"}
	return baseManifest("zcode", "0.1.0", "zcode_session", "zcode-json", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilitySupported), sameProcessDecl(CapabilityAttach, CapabilityDegraded), sameProcessDecl(CapabilitySnapshot, CapabilityDegraded), sameProcessDecl(CapabilityEvents, CapabilityDegraded), sameProcessDecl(CapabilityResult, CapabilitySupported), resultContent, capDecl(CapabilityResume, CapabilityUnavailable), sameProcessDecl(CapabilityCancel, CapabilitySupported), capDecl(CapabilityContextInjection, CapabilityDegraded),
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
	snapshot := capDecl(CapabilitySnapshot, CapabilityDegraded)
	snapshot.Constraints = map[string]any{"scope": "bound_issue", "cross_restart": true, "source": "native_cli"}
	cancel := capDecl(CapabilityCancel, CapabilitySupported)
	cancel.Constraints = map[string]any{"scope": "authority_forward", "cross_restart": true, "acceptance_semantics": "cancel_request_not_terminal"}
	m := baseManifest("multica", "0.1.0", "multica_run", "multica-json", []CapabilityDeclaration{
		capDecl(CapabilityLaunch, CapabilityUnavailable), capDecl(CapabilityAttach, CapabilityUnavailable), snapshot, capDecl(CapabilityEvents, CapabilityUnavailable), capDecl(CapabilityResult, CapabilityUnavailable), capDecl(CapabilityResultContent, CapabilityUnavailable), capDecl(CapabilityResume, CapabilityUnavailable), cancel, capDecl(CapabilityContextInjection, CapabilityDegraded),
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
	case "zcode":
		executable = "zcode"
	case "devin":
		executable = "devin"
	case "openclaw":
		executable = "openclaw"
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
		if declaredReason, ok := constraints["reason"].(string); ok {
			reason = declaredReason
		}
		if len(argv) != 0 && name == CapabilityResultContent && (!invocationRequirementSatisfied(argv, constraints) || manifest.Adapter == "openclaw" && !openclawInvocationSatisfied(argv)) {
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
