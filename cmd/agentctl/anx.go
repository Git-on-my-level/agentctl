package main

// ANX run links: optional, best-effort registration of labeled executions
// with Agent Nexus through the reviewed `anx` CLI. See
// docs/design/anx-run-links.md. Two invariants hold everywhere below:
//
//   - registration never blocks or fails a run: every failure defers to the
//     spool and surfaces only as a stderr note or `agentctl anx status`;
//   - the terminal read path is non-acknowledging: it opens the journal
//     read-only and uses GetOutcome, never `agentctl result` semantics.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/anxrun"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/route"
)

// anxSettings resolves the effective run-links configuration. A missing
// config file keeps the zero-config default (register labeled runs); an
// invalid config disables registration rather than guessing.
type anxSettings struct {
	AllowedOrgs    []string
	Catalog        []route.ModelRecord
	Mode           string
	WorkspaceAlias string
	Executable     string
	BaseURL        string
	ConfigDir      string
	SpoolDir       string
	FlushTimeout   time.Duration
}

func (s anxSettings) registers(labels []string) bool {
	switch s.Mode {
	case "off":
		return false
	case "all":
		return true
	default: // labeled
		return anxrun.Labeled(labels)
	}
}

func (a *app) anxSettings(c common) (anxSettings, bool) {
	settings := anxSettings{Mode: config.RunLinksDefaultMode, WorkspaceAlias: "anx", Executable: "anx", FlushTimeout: 10 * time.Second}
	journalPath, pathErr := a.journalPath(c)
	if pathErr != nil {
		return settings, false
	}
	settings.SpoolDir = filepath.Join(filepath.Dir(journalPath), "anx-run-links")
	resolution, err := configResolution(c)
	if err != nil {
		// An absent config keeps the zero-config default; a broken one
		// disables the hook rather than guessing effective settings.
		if !errors.Is(err, config.ErrNotFound) {
			return settings, false
		}
		return settings, true
	}
	if _, profile, err := resolution.Config.ResolveProfile(c.profile); err == nil {
		settings.Catalog = preferredRecords(profile)
	}
	if resolution.Config.Anx == nil || resolution.Config.Anx.RunLinks == nil {
		return settings, true
	}
	runLinks := *resolution.Config.Anx.RunLinks
	settings.AllowedOrgs = runLinks.AllowedOrgs
	if runLinks.Default != "" {
		settings.Mode = runLinks.Default
	}
	if runLinks.WorkspaceAlias != "" {
		settings.WorkspaceAlias = runLinks.WorkspaceAlias
	}
	if runLinks.Executable != "" {
		settings.Executable = runLinks.Executable
	}
	settings.BaseURL = runLinks.BaseURL
	settings.ConfigDir = runLinks.ConfigDir
	if runLinks.FlushTimeoutSecs > 0 {
		settings.FlushTimeout = time.Duration(runLinks.FlushTimeoutSecs) * time.Second
	}
	spoolDir := runLinks.SpoolDir
	if spoolDir == "" {
		spoolDir = settings.SpoolDir
	}
	settings.SpoolDir = spoolDir
	return settings, true
}

// anxOpenSpool opens the spool, reporting deferral (not failure) on error.
func (a *app) anxOpenSpool(settings anxSettings) (*anxrun.Spool, error) {
	spool, err := anxrun.OpenSpool(settings.SpoolDir)
	if err == nil {
		spool.Catalog = settings.Catalog
	}
	return spool, err
}

// anxSender builds the CLI sender for the resolved settings.
func anxSender(settings anxSettings) *anxrun.CLISender {
	return anxrun.NewCLISender(anxrun.CLIOptions{
		Catalog:        settings.Catalog,
		AllowedOrgs:    settings.AllowedOrgs,
		CacheDir:       settings.SpoolDir,
		Executable:     settings.Executable,
		WorkspaceAlias: settings.WorkspaceAlias,
		BaseURL:        settings.BaseURL,
		ConfigDir:      settings.ConfigDir,
		Timeout:        settings.FlushTimeout,
	})
}

// No unverified label or result text enters the spool. A delivery worker
// recovers candidates by generated execution ID and resolves authoritative
// cards and repository policy before the sender builds its public document.
func (a *app) anxJournalSender(c common, settings anxSettings) *anxrun.CLISender {
	sender := anxSender(settings)
	sender.Resolve = func(ctx context.Context, r anxrun.Registration) (anxrun.Registration, *anxrun.Repository, error) {
		journal, problem := a.openRead(c)
		if problem != nil {
			return r, nil, fmt.Errorf("local_io:0")
		}
		id, err := ids.ParseExecutionID(r.ExternalID)
		if err != nil {
			journal.Close()
			return r, nil, err
		}
		execution, err := journal.GetExecution(ctx, id)
		if err != nil {
			journal.Close()
			return r, nil, err
		}
		r.CardRefs = anxrun.ResolveCards(execution.Labels)
		if r.Phase == anxrun.PhaseTerminal {
			if outcome, err := journal.GetOutcome(ctx, id); err == nil {
				r.Outcome = anxrun.ProjectOutcome(outcome)
			}
		}
		journal.Close()
		var origin *anxrun.Repository
		r.CWD = ""
		if execution.CWD != nil {
			r.CWD = *execution.CWD
			origin = anxrun.OriginRepository(ctx, *execution.CWD)
		}
		return r, origin, nil
	}
	return sender
}

// anxRegisterStart spools the PhaseStart registration after an execution is
// journaled running. It performs no network I/O: launch never waits on ANX;
// a detached worker flushes the entry, as does a later launch or explicit
// `agentctl anx flush`.
func (a *app) anxRegisterStart(c common, execution model.Execution) {
	if execution.Authority != model.AuthorityNative && execution.Authority != model.AuthorityMultica {
		return
	}
	settings, ok := a.anxSettings(c)
	if !ok || !settings.registers(execution.Labels) {
		return
	}
	spool, err := a.anxOpenSpool(settings)
	if err != nil {
		a.anxNote("local_io:0")
		return
	}
	if _, err := spool.Enqueue(anxRegistrationModel(anxrun.BuildStart(execution, a.now()), execution)); err != nil {
		a.anxEgressNote(err)
	}
	a.scheduleANXFlush(c)
}

// anxRegisterTerminal spools the PhaseTerminal registration after a terminal
// commit and schedules a detached flush. The outcome is read through the
// journal's read-only outcome accessor — never through result dereference —
// so an uncollected run stays unreconciled for its parent.
func (a *app) anxRegisterTerminal(c common, id ids.ExecutionID) {
	settings, ok := a.anxSettings(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	journal, problem := a.openRead(c)
	if problem != nil {
		return
	}
	execution, err := journal.GetExecution(ctx, id)
	if err != nil {
		journal.Close()
		return
	}
	if !execution.State.Terminal() {
		journal.Close()
		return
	}
	// Non-acknowledging reads: the stored outcome and acknowledgement state
	// never pass through result dereference, so collection stays with the
	// parent's explicit `result`/`await` call.
	var outcome *model.Outcome
	if stored, outcomeErr := journal.GetOutcome(ctx, id); outcomeErr == nil {
		outcome = &stored
	}
	collected := true
	if _, ackErr := journal.GetAcknowledgement(ctx, id); ackErr != nil {
		collected = false
	}
	journal.Close()
	if !settings.registers(execution.Labels) {
		return
	}
	registration := anxRegistrationModel(anxrun.BuildTerminal(execution, outcome, collected, a.now()), execution)
	spool, err := a.anxOpenSpool(settings)
	if err != nil {
		a.anxNote("local_io:0")
		return
	}
	if _, err := spool.Enqueue(registration); err != nil {
		a.anxEgressNote(err)
		return
	}
	a.scheduleANXFlush(c)
}

func anxRegistrationModel(r anxrun.Registration, execution model.Execution) anxrun.Registration {
	if execution.Delegation != nil {
		r.Model = execution.Delegation.Resolved.Model
	}
	return r
}

// The return path only persists local state and starts a detached worker.
// Tests inject this seam; only newApp installs the production launcher.
func (a *app) scheduleANXFlush(c common) {
	if a.startANXWorker != nil {
		_ = a.startANXWorker(c)
	}
}

func startANXWorker(c common) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"_anx-flush-worker"}
	for _, flag := range []struct{ name, value string }{
		{"--journal", c.journalPath}, {"--config", c.configPath}, {"--config-bundle", c.configBundle}, {"--profile", c.profile}, {"--context", c.contextFile},
	} {
		if flag.value != "" {
			args = append(args, flag.name, flag.value)
		}
	}
	command := exec.Command(executable, args...)
	command.Env = os.Environ()
	prepareDetachedCommand(command)
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func (a *app) anxFlushWorker(ctx context.Context, c common) int {
	settings, ok := a.anxSettings(c)
	if !ok || settings.Mode == "off" {
		return 0
	}
	spool, present, err := anxrun.OpenSpoolIfPresent(settings.SpoolDir)
	if err != nil || !present {
		return 0
	}
	spool.Catalog = settings.Catalog
	_, _ = spool.Flush(ctx, a.anxJournalSender(c, settings), 0)
	return 0
}

func (a *app) anxEgressNote(err error) {
	class, code := anxrun.ErrorClass(err)
	if a.stderr != nil {
		fmt.Fprintf(a.stderr, "agentctl: anx run links: %s:%d\n", class, code)
	}
}

// anxNote writes one bounded stderr note. It never changes a command result.
func (a *app) anxNote(_ string) {
	if a.stderr != nil {
		fmt.Fprintln(a.stderr, "agentctl: anx run links: local_io:0")
	}
}

// anxCommand implements `agentctl anx status|flush`.
func (a *app) anxCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) != 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl anx status|flush", false)
	}
	settings, ok := a.anxSettings(c)
	if !ok {
		return output.NewError(output.CodeInvalidState, "configuration_invalid:0", false)
	}
	switch args[0] {
	case "status":
		return a.anxStatus(ctx, renderer, c, settings)
	case "flush":
		return a.anxFlush(ctx, renderer, c, settings)
	default:
		return output.NewError(output.CodeUsage, "usage: agentctl anx status|flush", false)
	}
}

type anxPendingItem struct {
	ExecutionID     string    `json:"execution_id"`
	Phase           string    `json:"phase"`
	CreatedAt       time.Time `json:"created_at"`
	Attempts        int       `json:"attempts"`
	NextAttempt     time.Time `json:"next_attempt_at,omitempty"`
	CardRefs        []string  `json:"card_refs,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	ErrorCode       int       `json:"error_code"`
	EgressErrorCode int       `json:"egress_error_code,omitempty"`
}

type anxDeadItem struct {
	ExecutionID     string    `json:"execution_id"`
	Phase           string    `json:"phase"`
	Reason          string    `json:"reason"`
	ErrorCode       int       `json:"error_code"`
	CreatedAt       time.Time `json:"created_at"`
	Attempts        int       `json:"attempts"`
	EgressErrorCode int       `json:"egress_error_code,omitempty"`
}

type anxStatusResult struct {
	Mode           string            `json:"mode"`
	WorkspaceAlias string            `json:"workspace_alias"`
	Spool          string            `json:"spool"`
	SpoolDir       string            `json:"spool_dir"`
	PendingCount   int               `json:"pending_count"`
	DeadCount      int               `json:"dead_letter_count"`
	Pending        []anxPendingItem  `json:"pending"`
	LastRejection  *anxrun.Rejection `json:"last_rejection,omitempty"`
	Dead           []anxDeadItem     `json:"dead_letters"`
}

func (a *app) anxStatus(ctx context.Context, renderer output.Renderer, c common, settings anxSettings) *output.Error {
	alias := "[redacted]"
	if anxrun.ValidWorkspaceAlias(settings.WorkspaceAlias) {
		alias = settings.WorkspaceAlias
	}
	mode := "off"
	switch settings.Mode {
	case "labeled", "all", "off":
		mode = settings.Mode
	}
	result := anxStatusResult{Mode: mode, WorkspaceAlias: alias, Spool: "absent", SpoolDir: anxrun.DisclosureValue(settings.SpoolDir), Pending: []anxPendingItem{}, Dead: []anxDeadItem{}}
	if spool, present, err := anxrun.OpenSpoolIfPresent(settings.SpoolDir); err == nil && present {
		result.Spool = "present"
		result.LastRejection = spool.LastRejection()
		if pending, err := spool.ListPending(); err == nil {
			for _, entry := range pending {
				result.Pending = append(result.Pending, anxPendingItem{ExecutionID: entry.ExecutionID, Phase: string(entry.Phase), CreatedAt: entry.CreatedAt, Attempts: entry.Attempts, NextAttempt: entry.NextAttemptAt, CardRefs: entry.Registration.CardRefs, LastError: entry.LastError, ErrorCode: entry.ErrorCode, EgressErrorCode: entry.Registration.EgressErrorCode})
			}
		}
		if dead, err := spool.ListDead(); err == nil {
			for _, entry := range dead {
				result.Dead = append(result.Dead, anxDeadItem{ExecutionID: entry.ExecutionID, Phase: string(entry.Phase), Reason: deadLetterReason(entry), ErrorCode: entry.ErrorCode, CreatedAt: entry.CreatedAt, Attempts: entry.Attempts, EgressErrorCode: entry.Registration.EgressErrorCode})
			}
		}
	}
	result.PendingCount, result.DeadCount = len(result.Pending), len(result.Dead)
	lines := []output.Line{{Lead: "anx", Fields: []output.Field{{Name: "mode", Value: result.Mode}, {Name: "workspace", Value: result.WorkspaceAlias}, {Name: "spool", Value: result.Spool}, {Name: "spool_dir", Value: result.SpoolDir}, {Name: "pending_count", Value: result.PendingCount}, {Name: "dead_letter_count", Value: result.DeadCount}}}}
	if result.LastRejection != nil {
		lines = append(lines, output.Line{Lead: result.LastRejection.Class, Fields: []output.Field{{Name: "code", Value: result.LastRejection.Code}}})
	}
	for _, item := range result.Pending {
		lines = append(lines, output.Line{Lead: "pending", Fields: []output.Field{{Name: "execution_id", Value: item.ExecutionID}, {Name: "phase", Value: item.Phase}, {Name: "attempts", Value: item.Attempts}, {Name: "next_attempt_at", Value: item.NextAttempt}}})
	}
	for _, item := range result.Dead {
		lines = append(lines, output.Line{Lead: "dead", Fields: []output.Field{{Name: "execution_id", Value: item.ExecutionID}, {Name: "phase", Value: item.Phase}, {Name: "reason", Value: item.Reason}}})
	}
	actions := []output.NextAction{{Label: "Flush pending registrations", Argv: []string{"agentctl", "anx", "flush"}, Mutates: true, SideEffectClass: output.RemoteCoordinationWrite, Preconditions: []string{}}}
	if err := renderer.Success(output.Success{Result: result, Lines: lines, NextActions: actions}); err != nil {
		return output.NewError(output.CodeInternal, "local_io:0", false)
	}
	return nil
}

func deadLetterReason(entry anxrun.Entry) string {
	if entry.LastError != "" {
		return entry.LastError
	}
	return "unknown"
}

type anxFlushResult struct {
	Delivered     int `json:"delivered"`
	Retried       int `json:"retried"`
	DeadLettered  int `json:"dead_lettered"`
	SkippedNotDue int `json:"skipped_not_due"`
}

func (a *app) anxFlush(ctx context.Context, renderer output.Renderer, c common, settings anxSettings) *output.Error {
	spool, err := a.anxOpenSpool(settings)
	if err != nil {
		return output.NewError(output.CodeInternal, "local_io:0", false)
	}
	flushCtx, cancel := context.WithTimeout(ctx, settings.FlushTimeout+5*time.Second)
	defer cancel()
	summary, err := spool.Flush(flushCtx, a.anxJournalSender(c, settings), 0)
	if err != nil {
		return output.NewError(output.CodeDependencyUnavailable, "anx_delivery_failed:0", true)
	}
	if err := spool.Cleanup(0); err != nil {
		a.anxNote("local_io:0")
	}
	result := anxFlushResult{Delivered: summary.Delivered, Retried: summary.Retried, DeadLettered: summary.DeadLettered, SkippedNotDue: summary.SkippedNotDue}
	lines := []output.Line{{Lead: "flush", Fields: []output.Field{{Name: "delivered", Value: result.Delivered}, {Name: "retried", Value: result.Retried}, {Name: "dead_lettered", Value: result.DeadLettered}, {Name: "skipped_not_due", Value: result.SkippedNotDue}}}}
	if err := renderer.Success(output.Success{Result: result, Lines: lines, NextActions: []output.NextAction{{Label: "Inspect remaining registrations", Argv: []string{"agentctl", "anx", "status"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}}}}); err != nil {
		return output.NewError(output.CodeInternal, "local_io:0", false)
	}
	return nil
}
