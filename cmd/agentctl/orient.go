package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const orientDefaultRecentLimit = 10

type orientGitRunner interface {
	Run(context.Context, string, ...string) (string, error)
}

type nativeOrientGit struct{}

func (nativeOrientGit) Run(ctx context.Context, dir string, args ...string) (string, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return "", errOrientGitUnavailable
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	// These commands are observations. In particular, status must not take the
	// optional index lock merely to refresh cached stat information.
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	combined, err := cmd.CombinedOutput()
	value := strings.TrimSpace(string(combined))
	if err != nil {
		return value, &orientGitError{Err: err, Diagnostic: value}
	}
	return value, nil
}

var errOrientGitUnavailable = errors.New("git executable unavailable")

type orientGitError struct {
	Err        error
	Diagnostic string
}

func (e *orientGitError) Error() string { return e.Err.Error() }
func (e *orientGitError) Unwrap() error { return e.Err }

type orientDependencies struct {
	getwd    func() (string, error)
	git      orientGitRunner
	lookPath func(string) (string, error)
	stat     func(string) (fs.FileInfo, error)
}

func defaultOrientDependencies() orientDependencies {
	return orientDependencies{getwd: os.Getwd, git: nativeOrientGit{}, lookPath: exec.LookPath, stat: os.Stat}
}

type orientUpstream struct {
	Status string `json:"status"`
	Name   string `json:"name,omitempty"`
	Ahead  int    `json:"ahead,omitempty"`
	Behind int    `json:"behind,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type orientWorkspace struct {
	Status         string         `json:"status"`
	CWD            string         `json:"cwd,omitempty"`
	RepositoryName string         `json:"repository_name,omitempty"`
	WorktreeRoot   string         `json:"worktree_root,omitempty"`
	GitDir         string         `json:"git_dir,omitempty"`
	CommonGitDir   string         `json:"common_git_dir,omitempty"`
	LinkedWorktree bool           `json:"linked_worktree"`
	Branch         string         `json:"branch,omitempty"`
	Detached       bool           `json:"detached"`
	Head           string         `json:"head,omitempty"`
	DirtyStatus    string         `json:"dirty_status"`
	Upstream       orientUpstream `json:"upstream"`
	Reason         string         `json:"reason,omitempty"`
}

type orientRoute struct {
	Status        string `json:"status"`
	ThisHost      string `json:"this_host,omitempty"`
	PlacementKind string `json:"placement_kind,omitempty"`
	KnownHosts    int    `json:"known_hosts"`
}

type orientConfiguration struct {
	Status      string      `json:"status"`
	Path        string      `json:"path,omitempty"`
	Profile     string      `json:"profile,omitempty"`
	Composition []string    `json:"composition_order"`
	Route       orientRoute `json:"route"`
	Reason      string      `json:"reason,omitempty"`
}

type orientAuthority struct {
	Kind                string `json:"kind"`
	ConfigurationStatus string `json:"configuration_status"`
	Health              string `json:"health"`
	HealthBasis         string `json:"health_basis"`
	Profile             string `json:"profile,omitempty"`
	WorkspaceID         string `json:"workspace_id,omitempty"`
	Reason              string `json:"reason,omitempty"`
}

type orientAdapter struct {
	Name                string `json:"name"`
	Authority           string `json:"authority"`
	ConfigurationStatus string `json:"configuration_status"`
	Executable          string `json:"executable,omitempty"`
	Health              string `json:"health"`
	HealthBasis         string `json:"health_basis"`
	Reason              string `json:"reason,omitempty"`
}

type orientExecution struct {
	ID         ids.ExecutionID `json:"id"`
	Labels     []string        `json:"labels"`
	Authority  model.Authority `json:"authority"`
	Adapter    string          `json:"adapter"`
	State      model.State     `json:"state"`
	Liveness   model.Liveness  `json:"liveness"`
	UpdatedAt  time.Time       `json:"updated_at"`
	TerminalAt *time.Time      `json:"terminal_at,omitempty"`
}

type orientExecutions struct {
	JournalStatus string            `json:"journal_status"`
	JournalPath   string            `json:"journal_path,omitempty"`
	MatchBasis    string            `json:"match_basis"`
	Matched       int               `json:"matched"`
	Unscoped      int               `json:"unscoped"`
	Active        []orientExecution `json:"active"`
	Recent        []orientExecution `json:"recent"`
	Reason        string            `json:"reason,omitempty"`
}

type orientProblem struct {
	Scope string `json:"scope"`
	Code  string `json:"code"`
}

type orientReport struct {
	Workspace     orientWorkspace     `json:"workspace"`
	Configuration orientConfiguration `json:"configuration"`
	Authorities   []orientAuthority   `json:"authorities"`
	Adapters      []orientAdapter     `json:"adapters"`
	Executions    orientExecutions    `json:"executions"`
	Problems      []orientProblem     `json:"problems"`
}

func (a *app) orient(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	limit, problem := parseOrient(args)
	if problem != nil {
		return problem
	}
	report := a.buildOrientation(ctx, c, limit, defaultOrientDependencies())
	lines := orientLines(report)
	actions := orientNextActions(report)
	if err := renderer.Success(output.Success{Result: report, Lines: lines, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write orientation", false, err)
	}
	return nil
}

func parseOrient(args []string) (int, *output.Error) {
	limit := orientDefaultRecentLimit
	for i := 0; i < len(args); i++ {
		if args[i] != "--limit" {
			return 0, output.NewError(output.CodeUsage, "unknown orient flag", false).WithDetail("flag", args[i])
		}
		if i+1 >= len(args) {
			return 0, output.NewError(output.CodeUsage, "--limit requires a value", false)
		}
		i++
		parsed, err := strconv.Atoi(strings.TrimSpace(args[i]))
		if err != nil || parsed < 1 || parsed > 100 {
			return 0, output.NewError(output.CodeUsage, "--limit must be between 1 and 100", false)
		}
		limit = parsed
	}
	return limit, nil
}

func (a *app) buildOrientation(ctx context.Context, c common, limit int, deps orientDependencies) orientReport {
	report := orientReport{Authorities: []orientAuthority{}, Adapters: []orientAdapter{}, Problems: []orientProblem{}}
	report.Workspace = inspectOrientWorkspace(ctx, deps)
	report.Configuration, report.Authorities, report.Adapters = inspectOrientConfiguration(c, deps)
	report.Executions = a.inspectOrientExecutions(ctx, c, report.Workspace, limit)

	if report.Workspace.Status == "unknown" {
		report.Problems = append(report.Problems, orientProblem{Scope: "workspace", Code: report.Workspace.Reason})
	}
	if report.Configuration.Status == "unhealthy" {
		report.Problems = append(report.Problems, orientProblem{Scope: "configuration", Code: report.Configuration.Reason})
	}
	for _, authority := range report.Authorities {
		if authority.Health == "unhealthy" {
			report.Problems = append(report.Problems, orientProblem{Scope: "authority:" + authority.Kind, Code: authority.Reason})
		}
	}
	for _, value := range report.Adapters {
		if value.Health == "unhealthy" {
			report.Problems = append(report.Problems, orientProblem{Scope: "adapter:" + value.Name, Code: value.Reason})
		}
	}
	if report.Executions.JournalStatus == "unhealthy" {
		report.Problems = append(report.Problems, orientProblem{Scope: "journal", Code: report.Executions.Reason})
	}
	sort.Slice(report.Problems, func(i, j int) bool {
		if report.Problems[i].Scope != report.Problems[j].Scope {
			return report.Problems[i].Scope < report.Problems[j].Scope
		}
		return report.Problems[i].Code < report.Problems[j].Code
	})
	return report
}

func inspectOrientWorkspace(ctx context.Context, deps orientDependencies) orientWorkspace {
	value := orientWorkspace{Status: "unknown", DirtyStatus: "unknown", Upstream: orientUpstream{Status: "unknown"}}
	cwd, err := deps.getwd()
	if err != nil {
		value.Reason = "cwd_unavailable"
		return value
	}
	value.CWD = filepath.Clean(cwd)
	inside, err := deps.git.Run(ctx, value.CWD, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if errors.Is(err, errOrientGitUnavailable) {
			value.Reason = "git_unavailable"
			return value
		}
		var commandErr *orientGitError
		if errors.As(err, &commandErr) && strings.Contains(strings.ToLower(commandErr.Diagnostic), "not a git repository") {
			value.Status = "not_repository"
			value.Reason = "not_a_git_repository"
			return value
		}
		value.Reason = "git_inspection_failed"
		return value
	}
	if inside != "true" {
		value.Status = "not_repository"
		value.Reason = "not_a_worktree"
		return value
	}
	root, err := deps.git.Run(ctx, value.CWD, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		value.Reason = "worktree_root_unavailable"
		return value
	}
	value.Status = "ready"
	value.WorktreeRoot = filepath.Clean(root)
	value.RepositoryName = filepath.Base(value.WorktreeRoot)
	if gitDir, gitErr := deps.git.Run(ctx, value.CWD, "rev-parse", "--absolute-git-dir"); gitErr == nil {
		value.GitDir = filepath.Clean(gitDir)
	}
	if commonDir, commonErr := deps.git.Run(ctx, value.CWD, "rev-parse", "--git-common-dir"); commonErr == nil {
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(value.CWD, commonDir)
		}
		if resolved, absoluteErr := filepath.Abs(commonDir); absoluteErr == nil {
			value.CommonGitDir = filepath.Clean(resolved)
		}
	}
	value.LinkedWorktree = value.GitDir != "" && value.CommonGitDir != "" && value.GitDir != value.CommonGitDir
	if head, headErr := deps.git.Run(ctx, value.CWD, "rev-parse", "HEAD"); headErr == nil {
		value.Head = head
	}
	if branch, branchErr := deps.git.Run(ctx, value.CWD, "symbolic-ref", "--quiet", "--short", "HEAD"); branchErr == nil && branch != "" {
		value.Branch = branch
	} else {
		value.Detached = true
	}
	if status, statusErr := deps.git.Run(ctx, value.CWD, "status", "--porcelain=v1", "--untracked-files=normal"); statusErr == nil {
		if status == "" {
			value.DirtyStatus = "clean"
		} else {
			value.DirtyStatus = "dirty"
		}
	}
	upstream, upstreamErr := deps.git.Run(ctx, value.CWD, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if upstreamErr != nil || upstream == "" {
		value.Upstream = orientUpstream{Status: "unconfigured", Reason: "upstream_unconfigured"}
		return value
	}
	value.Upstream = orientUpstream{Status: "unknown", Name: upstream, Reason: "divergence_unavailable"}
	counts, countsErr := deps.git.Run(ctx, value.CWD, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if countsErr != nil {
		return value
	}
	fields := strings.Fields(counts)
	if len(fields) != 2 {
		return value
	}
	ahead, aheadErr := strconv.Atoi(fields[0])
	behind, behindErr := strconv.Atoi(fields[1])
	if aheadErr != nil || behindErr != nil {
		return value
	}
	value.Upstream = orientUpstream{Status: "ready", Name: upstream, Ahead: ahead, Behind: behind}
	return value
}

func inspectOrientConfiguration(c common, deps orientDependencies) (orientConfiguration, []orientAuthority, []orientAdapter) {
	path, pathErr := configPath(c)
	result := orientConfiguration{Status: "unconfigured", Route: orientRoute{Status: "unconfigured"}, Composition: []string{}}
	if pathErr != nil {
		result.Status, result.Reason = "unhealthy", "config_path_unavailable"
		return result, unconfiguredAuthorities(), discoveredOrientAdapters(deps)
	}
	result.Path = path
	resolution, err := configResolution(c)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return result, unconfiguredAuthorities(), discoveredOrientAdapters(deps)
		}
		result.Status, result.Reason = "unhealthy", "configuration_invalid"
		return result, unconfiguredAuthorities(), discoveredOrientAdapters(deps)
	}
	profileName, profile, err := resolution.Config.ResolveProfile(c.profile)
	if err != nil {
		result.Status, result.Reason = "unhealthy", "profile_unavailable"
		result.Composition = append([]string{}, resolution.Composition...)
		return result, unconfiguredAuthorities(), discoveredOrientAdapters(deps)
	}
	result.Status = "ready"
	result.Profile = profileName
	result.Composition = append([]string{}, resolution.Composition...)
	if profile.Route != nil {
		result.Route = orientRoute{Status: "ready", ThisHost: profile.Route.ThisHost, KnownHosts: len(profile.Route.Hosts)}
		if profile.Route.Placement != nil {
			result.Route.PlacementKind = profile.Route.Placement.Kind
		}
	}

	adapters := make([]orientAdapter, 0, len(profile.Adapters)+1)
	for name, configured := range profile.Adapters {
		adapters = append(adapters, inspectOrientAdapter(name, "native", "configured", configured.Executable, deps))
	}
	if len(adapters) == 0 {
		adapters = discoveredOrientAdapters(deps)
	}
	sort.Slice(adapters, func(i, j int) bool { return adapters[i].Name < adapters[j].Name })
	nativeHealth, nativeReason := aggregateOrientHealth(adapters, "native")
	authorities := []orientAuthority{{Kind: "native", ConfigurationStatus: configuredStatus(len(profile.Adapters) > 0), Health: nativeHealth, HealthBasis: "local_executable_only", Profile: profileName, Reason: nativeReason}}
	if profile.Multica == nil {
		authorities = append(authorities, orientAuthority{Kind: "multica", ConfigurationStatus: "unconfigured", Health: "unconfigured", HealthBasis: "not_configured"})
	} else {
		multica := inspectOrientAdapter("multica", "multica", "configured", profile.Multica.Executable, deps)
		adapters = append(adapters, multica)
		sort.Slice(adapters, func(i, j int) bool { return adapters[i].Name < adapters[j].Name })
		authorityHealth, authorityReason := "unknown", "remote_authority_not_probed"
		if multica.Health == "unhealthy" {
			authorityHealth, authorityReason = "unhealthy", multica.Reason
		}
		authorities = append(authorities, orientAuthority{Kind: "multica", ConfigurationStatus: "configured", Health: authorityHealth, HealthBasis: "remote_auth_not_probed; local_executable_reported_separately", Profile: profileName, WorkspaceID: profile.Multica.WorkspaceID, Reason: authorityReason})
	}
	return result, authorities, adapters
}

func unconfiguredAuthorities() []orientAuthority {
	return []orientAuthority{
		{Kind: "native", ConfigurationStatus: "unconfigured", Health: "unknown", HealthBasis: "local_discovery_reported_per_adapter"},
		{Kind: "multica", ConfigurationStatus: "unconfigured", Health: "unconfigured", HealthBasis: "not_configured"},
	}
}

func discoveredOrientAdapters(deps orientDependencies) []orientAdapter {
	known := []struct{ name, executable string }{{"claude", "claude"}, {"codex", "codex"}, {"cursor", "cursor-agent"}, {"omp", "omp"}}
	values := make([]orientAdapter, 0, len(known))
	for _, item := range known {
		path, err := deps.lookPath(item.executable)
		if err != nil {
			values = append(values, orientAdapter{Name: item.name, Authority: "native", ConfigurationStatus: "unconfigured", Health: "unconfigured", HealthBasis: "not_configured", Reason: "executable_not_discovered"})
			continue
		}
		values = append(values, orientAdapter{Name: item.name, Authority: "native", ConfigurationStatus: "discovered", Executable: path, Health: "healthy", HealthBasis: "executable_present"})
	}
	return values
}

func inspectOrientAdapter(name, authority, configurationStatus, executable string, deps orientDependencies) orientAdapter {
	result := orientAdapter{Name: canonicalAdapterName(name), Authority: authority, ConfigurationStatus: configurationStatus, Executable: executable, Health: "unknown", HealthBasis: "not_checked"}
	if strings.TrimSpace(executable) == "" {
		result.Health, result.HealthBasis, result.Reason = "unhealthy", "configuration", "executable_unset"
		return result
	}
	if filepath.IsAbs(executable) || strings.ContainsRune(executable, filepath.Separator) {
		info, err := deps.stat(executable)
		if err != nil {
			result.Health, result.HealthBasis, result.Reason = "unhealthy", "executable_missing", "executable_unavailable"
			return result
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			result.Health, result.HealthBasis, result.Reason = "unhealthy", "executable_not_runnable", "executable_unavailable"
			return result
		}
		result.Health, result.HealthBasis = "healthy", "executable_present"
		return result
	}
	resolved, err := deps.lookPath(executable)
	if err != nil {
		result.Health, result.HealthBasis, result.Reason = "unhealthy", "path_lookup", "executable_unavailable"
		return result
	}
	result.Executable, result.Health, result.HealthBasis = resolved, "healthy", "executable_present"
	return result
}

func aggregateOrientHealth(adapters []orientAdapter, authority string) (string, string) {
	found, healthy, unhealthy := false, false, false
	for _, value := range adapters {
		if value.Authority != authority {
			continue
		}
		found = true
		healthy = healthy || value.Health == "healthy"
		unhealthy = unhealthy || value.Health == "unhealthy"
	}
	switch {
	case unhealthy:
		return "unhealthy", "adapter_executable_unavailable"
	case healthy:
		return "healthy", ""
	case found:
		return "unknown", "adapter_health_unknown"
	default:
		return "unconfigured", ""
	}
}

func configuredStatus(configured bool) string {
	if configured {
		return "configured"
	}
	return "unconfigured"
}

func (a *app) inspectOrientExecutions(ctx context.Context, c common, workspace orientWorkspace, limit int) orientExecutions {
	result := orientExecutions{JournalStatus: "unconfigured", MatchBasis: "cwd_or_repository_path", Active: []orientExecution{}, Recent: []orientExecution{}}
	path, err := a.journalPath(c)
	if err != nil {
		result.JournalStatus, result.Reason = "unhealthy", "journal_path_unavailable"
		return result
	}
	result.JournalPath = path
	if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
		return result
	} else if statErr != nil {
		result.JournalStatus, result.Reason = "unhealthy", "journal_unavailable"
		return result
	}
	journal, err := store.Open(path, store.Options{ReadOnly: true, LockTimeout: 100 * time.Millisecond})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return result
		}
		result.JournalStatus, result.Reason = "unhealthy", orientJournalReason(err)
		return result
	}
	defer journal.Close()
	result.JournalStatus = "ready"
	executions, err := journal.ListExecutions(ctx, false)
	if err != nil {
		result.JournalStatus, result.Reason = "unhealthy", "journal_read_failed"
		return result
	}
	root := workspace.WorktreeRoot
	if root == "" {
		root = workspace.CWD
	}
	matched := make([]model.Execution, 0)
	for _, execution := range executions {
		if execution.CWD == nil && execution.Repository == nil {
			result.Unscoped++
			continue
		}
		if orientExecutionMatches(execution, root) {
			matched = append(matched, execution)
		}
	}
	result.Matched = len(matched)
	for i := len(matched) - 1; i >= 0; i-- {
		projected := projectOrientExecution(matched[i])
		if !matched[i].State.Terminal() {
			result.Active = append(result.Active, projected)
		}
		if len(result.Recent) < limit {
			result.Recent = append(result.Recent, projected)
		}
	}
	return result
}

func orientJournalReason(err error) string {
	switch {
	case errors.Is(err, store.ErrBusy):
		return "journal_busy"
	case errors.Is(err, store.ErrPermission):
		return "journal_permission_denied"
	case errors.Is(err, store.ErrCorrupt):
		return "journal_corrupt"
	default:
		return "journal_unavailable"
	}
}

func orientExecutionMatches(execution model.Execution, root string) bool {
	if root == "" {
		return false
	}
	for _, candidate := range []*string{execution.CWD, execution.Repository} {
		if candidate == nil || !filepath.IsAbs(*candidate) {
			continue
		}
		if pathWithinOrientRoot(*candidate, root) {
			return true
		}
	}
	return false
}

func pathWithinOrientRoot(candidate, root string) bool {
	candidate = filepath.Clean(candidate)
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func projectOrientExecution(value model.Execution) orientExecution {
	labels := append([]string{}, value.Labels...)
	return orientExecution{ID: value.ID, Labels: labels, Authority: value.Authority, Adapter: value.Adapter, State: value.State, Liveness: value.Liveness, UpdatedAt: value.UpdatedAt, TerminalAt: value.TerminalAt}
}

func orientLines(report orientReport) []output.Line {
	lines := []output.Line{{Lead: "workspace", Fields: []output.Field{{Name: "status", Value: report.Workspace.Status}, {Name: "root", Value: report.Workspace.WorktreeRoot}, {Name: "branch", Value: report.Workspace.Branch}, {Name: "head", Value: report.Workspace.Head}, {Name: "dirty", Value: report.Workspace.DirtyStatus}, {Name: "upstream", Value: report.Workspace.Upstream.Status}, {Name: "ahead", Value: report.Workspace.Upstream.Ahead}, {Name: "behind", Value: report.Workspace.Upstream.Behind}}}, {Lead: "profile", Fields: []output.Field{{Name: "status", Value: report.Configuration.Status}, {Name: "name", Value: report.Configuration.Profile}, {Name: "route", Value: report.Configuration.Route.Status}, {Name: "this_host", Value: report.Configuration.Route.ThisHost}}}}
	for _, authority := range report.Authorities {
		lines = append(lines, output.Line{Lead: "authority", Fields: []output.Field{{Name: "kind", Value: authority.Kind}, {Name: "configured", Value: authority.ConfigurationStatus}, {Name: "health", Value: authority.Health}, {Name: "profile", Value: authority.Profile}, {Name: "workspace", Value: authority.WorkspaceID}}})
	}
	for _, value := range report.Adapters {
		lines = append(lines, output.Line{Lead: "adapter", Fields: []output.Field{{Name: "name", Value: value.Name}, {Name: "authority", Value: value.Authority}, {Name: "configured", Value: value.ConfigurationStatus}, {Name: "health", Value: value.Health}, {Name: "executable", Value: value.Executable}}})
	}
	lines = append(lines, output.Line{Lead: "executions", Fields: []output.Field{{Name: "journal", Value: report.Executions.JournalStatus}, {Name: "matched", Value: report.Executions.Matched}, {Name: "active", Value: len(report.Executions.Active)}, {Name: "recent", Value: len(report.Executions.Recent)}, {Name: "unscoped", Value: report.Executions.Unscoped}}})
	for _, value := range report.Executions.Active {
		lines = append(lines, output.Line{Lead: value.ID.String(), Fields: []output.Field{{Name: "scope", Value: "active"}, {Name: "adapter", Value: value.Adapter}, {Name: "state", Value: value.State}, {Name: "liveness", Value: value.Liveness}}})
	}
	for _, problem := range report.Problems {
		lines = append(lines, output.Line{Lead: "problem", Fields: []output.Field{{Name: "scope", Value: problem.Scope}, {Name: "code", Value: problem.Code}}})
	}
	return lines
}

func orientNextActions(report orientReport) []output.NextAction {
	actions := []output.NextAction{}
	if len(report.Executions.Active) > 0 {
		actions = append(actions, output.NextAction{Label: "Inspect newest workspace execution", Argv: []string{"agentctl", "status", report.Executions.Active[0].ID.String()}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	} else {
		actions = append(actions, output.NextAction{Label: "Inspect recent host-local executions", Argv: []string{"agentctl", "recent", "--state", "nonterminal"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	actions = append(actions, output.NextAction{Label: "Inspect readiness without launching adapters", Argv: []string{"agentctl", "doctor", "--static"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	return actions
}
