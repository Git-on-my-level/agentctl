package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

type doctorJournal struct {
	Path         string `json:"path"`
	Status       string `json:"status"`
	OriginHostID string `json:"origin_host_id,omitempty"`
}

type doctorConfig struct {
	Path             string                   `json:"path"`
	Status           string                   `json:"status"`
	Profile          string                   `json:"profile,omitempty"`
	AgentPreferences *config.AgentPreferences `json:"agent_preferences,omitempty"`
	Bundle           *config.BundleProvenance `json:"bundle,omitempty"`
	Composition      []string                 `json:"composition_order,omitempty"`
	Provenance       *config.ProvenanceReport `json:"provenance,omitempty"`
	Source           *config.SourceStatus     `json:"source,omitempty"`
	SourceError      string                   `json:"source_error,omitempty"`
}

type doctorAdapter struct {
	Name         string                         `json:"name"`
	Status       string                         `json:"status"`
	Executable   string                         `json:"executable,omitempty"`
	Capabilities map[adapter.CapabilityName]any `json:"capabilities"`
	Probe        *adapter.ProbeResult           `json:"probe,omitempty"`
	Error        map[string]any                 `json:"error,omitempty"`
}

type doctorReadinessReport struct {
	Healthy    bool            `json:"healthy"`
	Bootstrap  bootstrapStatus `json:"bootstrap"`
	Journal    doctorJournal   `json:"journal"`
	Config     doctorConfig    `json:"config"`
	Supervisor map[string]any  `json:"supervisor"`
	Adapters   []doctorAdapter `json:"adapters"`
	Problems   []string        `json:"problems"`
}

func (a *app) doctorReadiness(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	selected, static, full := []string{}, false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--adapter":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--adapter requires a value", false)
			}
			i++
			selected = append(selected, splitValues(args[i])...)
		case "--static":
			static = true
		case "--full":
			full = true
		default:
			return output.NewError(output.CodeUsage, "unknown doctor flag", false).WithDetail("flag", args[i])
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return output.Wrap(output.CodeDependencyUnavailable, "resolve home directory", false, err)
	}
	expected, problem := parseExpectedHarnesses(home, "")
	if problem != nil {
		return problem
	}
	bootstrap := buildBootstrapStatus(home, expected, a.getenv)
	report := doctorReadinessReport{Healthy: bootstrap.Healthy, Bootstrap: bootstrap, Problems: append([]string(nil), bootstrap.Problems...), Adapters: []doctorAdapter{}, Supervisor: map[string]any{"status": "absent", "required": false}}

	journalPath, err := a.journalPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve journal path", false, err)
	}
	report.Journal = doctorJournal{Path: journalPath, Status: "absent_ready_to_create"}
	if journal, journalErr := store.Open(journalPath, store.Options{ReadOnly: true}); journalErr == nil {
		host, hostErr := journal.HostID(ctx)
		_ = journal.Close()
		if hostErr != nil {
			return mapStoreError("read journal host", hostErr)
		}
		report.Journal.Status, report.Journal.OriginHostID = "ready", host.String()
	} else if !errors.Is(journalErr, store.ErrNotFound) {
		return mapStoreError("inspect journal", journalErr)
	}
	if bootstrap.SupervisorSocket {
		report.Supervisor["status"] = "running"
		report.Supervisor["socket"] = true
	}
	// A reachable socket only proves the process is listening. Readiness has to
	// come from the service's own health projection, or doctor reports healthy
	// while deliveries are stuck.
	supervisorProblems := []string{}
	if !static {
		supervisorProblems = a.foldSupervisorHealth(ctx, report.Supervisor, bootstrap.SupervisorSocket)
		if len(supervisorProblems) != 0 {
			report.Healthy = false
			report.Problems = append(report.Problems, supervisorProblems...)
		}
	}

	configPath := c.configPath
	if configPath == "" {
		configPath, err = config.DefaultPath()
		if err != nil {
			return output.Wrap(output.CodeInternal, "resolve config path", false, err)
		}
	}
	report.Config = doctorConfig{Path: configPath, Status: "absent_optional"}
	configuredExecutables := map[string]string{}
	configuredAdapters := []string{}
	if resolution, configErr := config.Resolve(configPath, c.configBundle); configErr == nil {
		cfg := resolution.Config
		name, profile, resolveErr := cfg.ResolveProfile(c.profile)
		if resolveErr != nil {
			return mapConfigError("profile is unavailable", resolveErr)
		}
		provenance, provenanceErr := config.Doctor(ctx, cfg, c.profile)
		report.Config = doctorConfig{Path: configPath, Status: "ready", Profile: name, AgentPreferences: profile.AgentPreferences, Bundle: resolution.Bundle, Composition: resolution.Composition}
		if full {
			report.Config.Provenance = &provenance
		}
		if provenanceErr != nil {
			report.Healthy = false
			report.Problems = append(report.Problems, "config_provenance_failed")
		}
		for name, value := range profile.Adapters {
			configuredExecutables[canonicalAdapterName(name)] = value.Executable
			configuredAdapters = append(configuredAdapters, name)
		}
		if profile.Multica != nil {
			configuredAdapters = append(configuredAdapters, "multica")
			configuredExecutables["multica"] = profile.Multica.Executable
		}
	} else if !errors.Is(configErr, config.ErrNotFound) {
		return output.Wrap(output.CodeUsage, "invalid config", false, configErr)
	}
	if sourceStatus, sourceErr := config.SourceStatusReadOnly(ctx, configPath); sourceErr != nil {
		report.Healthy = false
		report.Problems = append(report.Problems, "config_source_invalid")
		report.Config.SourceError = sourceErr.Error()
	} else if sourceStatus.Configured {
		report.Config.Source = &sourceStatus
		if !sourceStatus.InSync {
			report.Healthy = false
			report.Problems = append(report.Problems, "config_source_drift")
		}
	}

	selected = doctorAdapterSelection(selected, configuredAdapters, expected)
	for _, name := range selected {
		value, adapterProblem := a.adapterForIntrospection(name, c)
		if adapterProblem != nil {
			report.Healthy = false
			report.Problems = append(report.Problems, name+"_unavailable")
			report.Adapters = append(report.Adapters, doctorAdapter{Name: name, Status: "unavailable", Capabilities: map[adapter.CapabilityName]any{}, Error: map[string]any{"code": adapterProblem.Code, "message": adapterProblem.Message}})
			continue
		}
		check := doctorAdapter{Name: value.Name(), Status: "ready", Capabilities: manifestCapabilityProjection(value.Manifest())}
		if !full {
			check.Capabilities = doctorCapabilitySummary(name, check.Capabilities)
		}
		if static {
			if !doctorManifestReady(name, value.Manifest()) {
				check.Status = "degraded"
				report.Healthy = false
				report.Problems = append(report.Problems, name+"_delegation_unavailable")
			}
		} else {
			probe, probeErr := value.Probe(ctx, adapter.ProbeRequest{Executable: configuredExecutables[name], Profile: c.profile, Timeout: 5 * time.Second, Fresh: true})
			if probeErr != nil {
				mapped := mapAdapterError("adapter probe failed", probeErr)
				check.Status = "unavailable"
				check.Error = map[string]any{"code": mapped.Code, "message": mapped.Message}
				report.Healthy = false
				report.Problems = append(report.Problems, name+"_probe_failed")
			} else {
				check.Executable = probe.Executable
				if full {
					check.Probe = &probe
				}
				check.Capabilities = probeCapabilityProjection(probe)
				if !full {
					check.Capabilities = doctorCapabilitySummary(name, check.Capabilities)
				}
				if !doctorProbeReady(name, probe) {
					check.Status = "degraded"
					report.Healthy = false
					report.Problems = append(report.Problems, name+"_delegation_unavailable")
				}
			}
		}
		report.Adapters = append(report.Adapters, check)
	}
	sort.Strings(report.Problems)
	report.Problems = uniqueStrings(report.Problems)
	lines := []output.Line{{Lead: "doctor", Fields: []output.Field{{Name: "healthy", Value: report.Healthy}, {Name: "journal", Value: report.Journal.Status}, {Name: "config", Value: report.Config.Status}, {Name: "supervisor", Value: report.Supervisor["status"]}, {Name: "adapters", Value: len(report.Adapters)}}}}
	for _, value := range report.Adapters {
		lines = append(lines, output.Line{Lead: "adapter", Fields: []output.Field{{Name: "name", Value: value.Name}, {Name: "status", Value: value.Status}, {Name: "executable", Value: value.Executable}}})
	}
	actions := []output.NextAction{}
	if len(supervisorProblems) != 0 {
		actions = append(actions, output.NextAction{Label: "Inspect supervisor health", Argv: []string{"agentctl", "supervisor", "status"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	if !bootstrap.Healthy {
		actions = append(actions, output.NextAction{Label: "Reconcile detected installations", Argv: []string{"agentctl", "bootstrap", "update", "--dry-run"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	if report.Config.Source != nil && !report.Config.Source.InSync {
		actions = append(actions, output.NextAction{Label: "Inspect config source drift", Argv: []string{"agentctl", "config", "source", "status"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
		if canRestore, blockers := sourceRestorePlan(*report.Config.Source); canRestore && len(blockers) == 0 {
			actions = append(actions, output.NextAction{Label: "Plan live config restore", Argv: []string{"agentctl", "config", "source", "restore", "--plan"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
		}
	}
	if err := renderer.Success(output.Success{Result: report, Lines: lines, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func doctorAdapterSelection(explicit, configured, detected []string) []string {
	if len(explicit) != 0 {
		return uniqueAdapterNames(explicit)
	}
	selected := append([]string(nil), configured...)
	for _, name := range detected {
		if name != "hermes" {
			selected = append(selected, name)
		}
	}
	return uniqueAdapterNames(selected)
}

func canonicalAdapterName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "claude-code":
		return "claude"
	case "generic":
		return "generic-process"
	default:
		return value
	}
}

func uniqueAdapterNames(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, raw := range values {
		value := canonicalAdapterName(raw)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func manifestCapabilityProjection(manifest adapter.Manifest) map[adapter.CapabilityName]any {
	result := map[adapter.CapabilityName]any{}
	for _, value := range manifest.Capabilities {
		result[value.Name] = map[string]any{"status": value.Implementation, "constraints": value.Constraints}
	}
	return result
}

func probeCapabilityProjection(probe adapter.ProbeResult) map[adapter.CapabilityName]any {
	result := map[adapter.CapabilityName]any{}
	for _, value := range probe.Capabilities {
		result[value.Name] = map[string]any{"status": value.Status, "constraints": value.Constraints}
	}
	return result
}

func doctorCapabilitySummary(name string, values map[adapter.CapabilityName]any) map[adapter.CapabilityName]any {
	required := []adapter.CapabilityName{adapter.CapabilityLaunch, adapter.CapabilityResultContent}
	if name == "multica" {
		required = []adapter.CapabilityName{adapter.CapabilityEvents}
	}
	result := map[adapter.CapabilityName]any{}
	for _, capability := range required {
		if value, ok := values[capability]; ok {
			result[capability] = value
		}
	}
	return result
}

func manifestCapabilityReady(manifest adapter.Manifest, name adapter.CapabilityName) bool {
	for _, value := range manifest.Capabilities {
		if value.Name == name {
			return value.Implementation != adapter.CapabilityUnavailable
		}
	}
	return false
}

func doctorManifestReady(name string, manifest adapter.Manifest) bool {
	if name == "multica" {
		return manifestCapabilityReady(manifest, adapter.CapabilityEvents)
	}
	return manifestCapabilityReady(manifest, adapter.CapabilityLaunch) && manifestCapabilityReady(manifest, adapter.CapabilityResultContent)
}

func doctorProbeReady(name string, probe adapter.ProbeResult) bool {
	if name == "multica" {
		return capabilityReady(probe, adapter.CapabilityEvents)
	}
	return capabilityReady(probe, adapter.CapabilityLaunch) && capabilityReady(probe, adapter.CapabilityResultContent)
}

func capabilityReady(probe adapter.ProbeResult, name adapter.CapabilityName) bool {
	for _, value := range probe.Capabilities {
		if value.Name == name {
			return value.Status != adapter.CapabilityUnavailable
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

// doctorSupervisorCycleStaleAfter bounds how old the supervisor's last cycle may
// be before doctor stops calling it healthy. It is twelve default cycles, so a
// briefly busy host does not read as a stalled service.
const doctorSupervisorCycleStaleAfter = time.Minute

// foldSupervisorHealth folds the service's own health projection into doctor's
// supervisor block. Socket reachability alone proves only that something is
// listening: degraded state, a recorded last error, or a stale cycle must make
// the readiness report unhealthy rather than silently healthy. It returns the
// problem codes it discovered and performs no write.
func (a *app) foldSupervisorHealth(ctx context.Context, block map[string]any, socketPresent bool) []string {
	probe := a.supervisorHealthProbe
	if probe == nil {
		// An absent socket stays an absent optional service: doctor must not
		// dial, and must not invent a problem for a supervisor nobody installed.
		if !socketPresent {
			return nil
		}
		probe = a.dialSupervisorStatus
	}
	status, err := probe(ctx)
	if err != nil {
		// A reachable-looking socket that refuses a bounded read-only status
		// request is a real readiness problem, not an absent service.
		block["status"] = "unreachable"
		block["socket"] = true
		block["error"] = err.Error()
		return []string{"supervisor_unreachable"}
	}
	health := status.Health
	block["status"] = string(health.State)
	block["health_state"] = string(health.State)
	block["running"] = status.Running
	block["pending_deliveries"] = health.PendingDeliveries
	block["nonterminal_executions"] = health.NonTerminalExecutions
	if health.LastError != "" {
		block["last_error"] = health.LastError
	}
	problems := []string{}
	if !health.LastCycleAt.IsZero() {
		block["last_cycle_at"] = health.LastCycleAt.UTC()
		age := a.now().UTC().Sub(health.LastCycleAt.UTC())
		if age < 0 {
			age = 0
		}
		block["last_cycle_age_seconds"] = age.Seconds()
		if age > doctorSupervisorCycleStaleAfter {
			block["cycle_freshness"] = "stale"
			problems = append(problems, "supervisor_cycle_stale")
		} else {
			block["cycle_freshness"] = "fresh"
		}
	} else {
		block["cycle_freshness"] = "unknown"
	}
	switch health.State {
	case supervisor.HealthHealthy, supervisor.HealthStarting:
	default:
		problems = append(problems, "supervisor_"+string(health.State))
	}
	if health.LastError != "" && len(problems) == 0 {
		problems = append(problems, "supervisor_last_error")
	}
	return problems
}

// dialSupervisorStatus performs one bounded read-only status RPC against the
// owner-only Unix socket. It never starts, installs, or restarts a service.
func (a *app) dialSupervisorStatus(ctx context.Context) (supervisor.Status, error) {
	socketPath, problem := a.defaultSupervisorSocket()
	if problem != nil {
		return supervisor.Status{}, problem
	}
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socketPath)
	if err != nil {
		return supervisor.Status{}, err
	}
	defer conn.Close()
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(supervisor.RPCRequest{Op: "status"}); err != nil {
		return supervisor.Status{}, err
	}
	var response supervisor.RPCResponse
	if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&response); err != nil {
		return supervisor.Status{}, err
	}
	if !response.OK || response.Status == nil {
		return supervisor.Status{}, errors.New("supervisor rejected the status request")
	}
	return *response.Status, nil
}
