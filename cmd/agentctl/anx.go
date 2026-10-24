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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/anxrun"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

// anxSettings resolves the effective run-links configuration. A missing
// config file keeps the zero-config default (register labeled runs); an
// invalid config disables registration rather than guessing.
type anxSettings struct {
	Mode            string
	WorkspaceAlias  string
	Executable      string
	BaseURL         string
	ConfigDir       string
	SummaryMaxBytes int
	SpoolDir        string
	FlushTimeout    time.Duration
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
	settings := anxSettings{Mode: config.RunLinksDefaultMode, WorkspaceAlias: "anx", Executable: "anx", SummaryMaxBytes: anxrun.DefaultSummaryMaxBytes, FlushTimeout: 10 * time.Second}
	resolution, err := configResolution(c)
	if err != nil {
		// An absent config keeps the zero-config default; a broken one
		// disables the hook rather than guessing effective settings.
		if !errors.Is(err, config.ErrNotFound) {
			return settings, false
		}
		return settings, true
	}
	if resolution.Config.Anx == nil || resolution.Config.Anx.RunLinks == nil {
		return settings, true
	}
	runLinks := *resolution.Config.Anx.RunLinks
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
	if runLinks.SummaryMaxBytes > 0 {
		settings.SummaryMaxBytes = runLinks.SummaryMaxBytes
	}
	if runLinks.FlushTimeoutSecs > 0 {
		settings.FlushTimeout = time.Duration(runLinks.FlushTimeoutSecs) * time.Second
	}
	spoolDir := runLinks.SpoolDir
	if spoolDir == "" {
		journalPath, pathErr := a.journalPath(c)
		if pathErr != nil {
			return settings, false
		}
		spoolDir = filepath.Join(filepath.Dir(journalPath), "anx-run-links")
	}
	settings.SpoolDir = spoolDir
	return settings, true
}

// anxOpenSpool opens the spool, reporting deferral (not failure) on error.
func (a *app) anxOpenSpool(settings anxSettings) (*anxrun.Spool, error) {
	return anxrun.OpenSpool(settings.SpoolDir)
}

// anxSender builds the CLI sender for the resolved settings.
func anxSender(settings anxSettings) *anxrun.CLISender {
	return anxrun.NewCLISender(anxrun.CLIOptions{
		Executable:     settings.Executable,
		WorkspaceAlias: settings.WorkspaceAlias,
		BaseURL:        settings.BaseURL,
		ConfigDir:      settings.ConfigDir,
		Timeout:        settings.FlushTimeout,
	})
}

// anxRegisterStart spools the PhaseStart registration after an execution is
// journaled running. It performs no network I/O: launch never waits on ANX;
// the entry flushes on the terminal hook, a later launch, or an explicit
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
		a.anxNote("registration deferred: %v", err)
		return
	}
	if _, err := spool.Enqueue(anxrun.BuildStart(execution, a.now())); err != nil {
		a.anxNote("registration deferred: %v", err)
	}
}

// anxRegisterTerminal spools the PhaseTerminal registration after a terminal
// commit and attempts one bounded flush. The outcome is read through the
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
	registration := anxrun.BuildTerminal(execution, outcome, collected, a.now())
	if outcome != nil {
		// Re-project with the configured summary bound; BuildTerminal used
		// the default.
		registration.Outcome = anxrun.ProjectOutcome(*outcome, settings.SummaryMaxBytes)
	}
	if registration.Outcome != nil {
		if cwd := execution.CWD; cwd != nil && *cwd != "" {
			registration.Outcome.HeadAfter = gitHeadAfter(ctx, *cwd)
		}
	}
	spool, err := a.anxOpenSpool(settings)
	if err != nil {
		a.anxNote("registration deferred: %v", err)
		return
	}
	if _, err := spool.Enqueue(registration); err != nil {
		a.anxNote("registration deferred: %v", err)
		return
	}
	flushCtx, flushCancel := context.WithTimeout(context.Background(), settings.FlushTimeout)
	defer flushCancel()
	if _, err := spool.Flush(flushCtx, anxSender(settings), 0); err != nil {
		a.anxNote("flush deferred: %v", err)
	}
}

// anxNote writes one bounded stderr note. It never changes a command result.
func (a *app) anxNote(format string, args ...any) {
	if a.stderr == nil {
		return
	}
	message := fmt.Sprintf(format, args...)
	if len(message) > 200 {
		message = message[:200]
	}
	fmt.Fprintf(a.stderr, "agentctl: anx run links: %s\n", message)
}

// headAfterGitTimeout bounds the git HEAD read for head_after.
const headAfterGitTimeout = 2 * time.Second

// gitHeadAfter reads the bare 40-hex HEAD of cwd, best-effort.
func gitHeadAfter(ctx context.Context, cwd string) string {
	if cwd == "" {
		return ""
	}
	readCtx, cancel := context.WithTimeout(ctx, headAfterGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(readCtx, "git", "-C", cwd, "rev-parse", "HEAD")
	outputBytes, err := cmd.Output()
	if err != nil {
		return ""
	}
	head, err := anxrun.SanitizeHeadAfter(strings.TrimSpace(string(outputBytes)))
	if err != nil {
		return ""
	}
	return head
}

// anxCommand implements `agentctl anx status|flush`.
func (a *app) anxCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) != 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl anx status|flush", false)
	}
	settings, ok := a.anxSettings(c)
	if !ok {
		return output.NewError(output.CodeInvalidState, "anx configuration is invalid; registration is disabled until the config is repaired", false)
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
	ExecutionID string    `json:"execution_id"`
	Phase       string    `json:"phase"`
	CreatedAt   time.Time `json:"created_at"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt_at,omitempty"`
	CardRefs    []string  `json:"card_refs,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

type anxDeadItem struct {
	ExecutionID string    `json:"execution_id"`
	Phase       string    `json:"phase"`
	Reason      string    `json:"reason"`
	CreatedAt   time.Time `json:"created_at"`
	Attempts    int       `json:"attempts"`
}

type anxStatusResult struct {
	Mode           string           `json:"mode"`
	WorkspaceAlias string           `json:"workspace_alias"`
	SpoolDir       string           `json:"spool_dir"`
	Pending        []anxPendingItem `json:"pending"`
	Dead           []anxDeadItem    `json:"dead_letters"`
}

func (a *app) anxStatus(ctx context.Context, renderer output.Renderer, c common, settings anxSettings) *output.Error {
	result := anxStatusResult{Mode: settings.Mode, WorkspaceAlias: settings.WorkspaceAlias, SpoolDir: settings.SpoolDir, Pending: []anxPendingItem{}, Dead: []anxDeadItem{}}
	if spool, present, err := anxrun.OpenSpoolIfPresent(settings.SpoolDir); err == nil && present {
		if pending, err := spool.ListPending(); err == nil {
			for _, entry := range pending {
				result.Pending = append(result.Pending, anxPendingItem{ExecutionID: entry.ExecutionID, Phase: string(entry.Phase), CreatedAt: entry.CreatedAt, Attempts: entry.Attempts, NextAttempt: entry.NextAttemptAt, CardRefs: entry.Registration.CardRefs, LastError: entry.LastError})
			}
		}
		if dead, err := spool.ListDead(); err == nil {
			for _, entry := range dead {
				result.Dead = append(result.Dead, anxDeadItem{ExecutionID: entry.ExecutionID, Phase: string(entry.Phase), Reason: deadLetterReason(entry), CreatedAt: entry.CreatedAt, Attempts: entry.Attempts})
			}
		}
	}
	lines := []output.Line{{Lead: "anx", Fields: []output.Field{{Name: "mode", Value: result.Mode}, {Name: "workspace", Value: result.WorkspaceAlias}, {Name: "spool", Value: result.SpoolDir}}}}
	for _, item := range result.Pending {
		lines = append(lines, output.Line{Lead: "pending", Fields: []output.Field{{Name: "execution_id", Value: item.ExecutionID}, {Name: "phase", Value: item.Phase}, {Name: "attempts", Value: item.Attempts}, {Name: "next_attempt_at", Value: item.NextAttempt}}})
	}
	for _, item := range result.Dead {
		lines = append(lines, output.Line{Lead: "dead", Fields: []output.Field{{Name: "execution_id", Value: item.ExecutionID}, {Name: "phase", Value: item.Phase}, {Name: "reason", Value: item.Reason}}})
	}
	actions := []output.NextAction{{Label: "Flush pending registrations", Argv: []string{"agentctl", "anx", "flush"}, Mutates: true, SideEffectClass: output.RemoteCoordinationWrite, Preconditions: []string{}}}
	if err := renderer.Success(output.Success{Result: result, Lines: lines, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write anx status", false, err)
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
		return output.Wrap(output.CodeInternal, "open anx spool", false, err)
	}
	flushCtx, cancel := context.WithTimeout(ctx, settings.FlushTimeout+5*time.Second)
	defer cancel()
	summary, err := spool.Flush(flushCtx, anxSender(settings), 0)
	if err != nil {
		return output.Wrap(output.CodeDependencyUnavailable, "flush anx registrations", true, err)
	}
	if err := spool.Cleanup(0); err != nil {
		a.anxNote("dead-letter cleanup deferred: %v", err)
	}
	result := anxFlushResult{Delivered: summary.Delivered, Retried: summary.Retried, DeadLettered: summary.DeadLettered, SkippedNotDue: summary.SkippedNotDue}
	lines := []output.Line{{Lead: "flush", Fields: []output.Field{{Name: "delivered", Value: result.Delivered}, {Name: "retried", Value: result.Retried}, {Name: "dead_lettered", Value: result.DeadLettered}, {Name: "skipped_not_due", Value: result.SkippedNotDue}}}}
	if err := renderer.Success(output.Success{Result: result, Lines: lines, NextActions: []output.NextAction{{Label: "Inspect remaining registrations", Argv: []string{"agentctl", "anx", "status"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write anx flush result", false, err)
	}
	return nil
}
