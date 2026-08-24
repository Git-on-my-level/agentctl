package main

import (
	"context"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func (a *app) capabilitiesCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "usage: agentctl capabilities <adapter> [--require names] [--full] [--static] [--executable path]", false)
	}
	name := strings.ToLower(strings.TrimSpace(args[0]))
	probe := true
	summary := true
	required := []string{}
	executable := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--probe":
			probe = true
		case "--summary":
			summary = true
		case "--full":
			summary = false
		case "--static":
			probe = false
		case "--require":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--require requires a comma-separated capability list", false)
			}
			i++
			required = append(required, splitValues(args[i])...)
		case "--executable":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--executable requires a value", false)
			}
			i++
			executable = args[i]
		default:
			return output.NewError(output.CodeUsage, "unknown capabilities flag", false).WithDetail("flag", args[i])
		}
	}
	value, problem := a.adapterForIntrospection(name, c)
	if problem != nil {
		return problem
	}
	manifest := value.Manifest()
	result := map[string]any{"manifest": manifest}
	lines := []output.Line{{Lead: "adapter", Fields: []output.Field{{Name: "name", Value: manifest.Adapter}, {Name: "version", Value: manifest.AdapterVersion}, {Name: "capabilities", Value: len(manifest.Capabilities)}}}}
	// A generic process has no executable discovery rule of its own. Its
	// summary remains static unless the caller supplies the exact executable.
	if probe && (name != "generic" && name != "generic-process" || executable != "") {
		observation, err := value.Probe(ctx, adapter.ProbeRequest{Executable: executable, Profile: c.profile, Fresh: true})
		if err != nil {
			return mapAdapterError("adapter probe failed", err).WithDetail("adapter", name)
		}
		result["probe"] = observation
		lines = append(lines, output.Line{Lead: "probe", Fields: []output.Field{{Name: "adapter", Value: manifest.Adapter}, {Name: "version", Value: observation.AdapterVersion}, {Name: "capabilities", Value: len(observation.Capabilities)}}})
	}
	if summary || len(required) > 0 {
		statuses := map[string]adapter.CapabilityStatus{}
		constraints := map[string]map[string]any{}
		for _, item := range manifest.Capabilities {
			status := item.Implementation
			if status == adapter.CapabilityConditional {
				status = adapter.CapabilityDegraded
			}
			statuses[string(item.Name)] = status
			constraints[string(item.Name)] = item.Constraints
		}
		var probedAt any
		freshForSeconds := 0
		if observationValue, ok := result["probe"].(adapter.ProbeResult); ok {
			for _, item := range observationValue.Capabilities {
				statuses[string(item.Name)] = item.Status
				constraints[string(item.Name)] = item.Constraints
			}
			probedAt = observationValue.ProbedAt
			freshForSeconds = int(observationValue.FreshFor / time.Second)
		}
		blocking, degraded := []string{}, []string{}
		selected := map[string]any{}
		for _, name := range required {
			status, ok := statuses[name]
			if !ok || status == adapter.CapabilityUnavailable {
				blocking = append(blocking, name)
			} else if status == adapter.CapabilityDegraded {
				degraded = append(degraded, name)
			}
			selected[name] = map[string]any{"status": status, "constraints": constraints[name]}
		}
		if len(required) == 0 {
			for name, status := range statuses {
				selected[name] = map[string]any{"status": status, "constraints": constraints[name]}
			}
		}
		result = map[string]any{"adapter": manifest.Adapter, "adapter_version": manifest.AdapterVersion, "viable": len(blocking) == 0, "required": selected, "degraded": degraded, "blocking": blocking, "probed_at": probedAt, "fresh_for_seconds": freshForSeconds}
		if len(blocking) > 0 {
			return output.NewError(output.CodeCapabilityUnavailable, "required adapter capabilities are unavailable", false).WithDetail("adapter", manifest.Adapter).WithDetail("blocking", blocking)
		}
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func (a *app) adapterForIntrospection(name string, c common) (adapter.Adapter, *output.Error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "generic", "generic-process":
		return adapter.NewGenericProcess(), nil
	case "codex":
		return adapter.NewCodex(), nil
	case "cursor":
		return adapter.NewCursor(), nil
	case "claude", "claude-code":
		return adapter.NewClaude(), nil
	case "omp":
		return adapter.NewOMP(), nil
	case "multica":
		resolution, err := configResolution(c)
		if err != nil {
			return nil, mapConfigError("read exact Multica authority", err)
		}
		_, profile, err := resolution.Config.ResolveProfile(c.profile)
		if err != nil {
			return nil, mapConfigError("resolve exact Multica profile", err)
		}
		if profile.Multica == nil {
			return nil, output.NewError(output.CodeCapabilityUnavailable, "selected profile has no Multica authority", false)
		}
		m := profile.Multica
		return adapter.NewMultica(adapter.MulticaConfig{Binary: m.Executable, Profile: m.Profile, Endpoint: m.ServerURL, Workspace: m.WorkspaceID}), nil
	default:
		return nil, output.NewError(output.CodeUsage, "unknown adapter", false).WithDetail("adapter", name)
	}
}

func (a *app) schemaCommand(renderer output.Renderer, args []string) *output.Error {
	if len(args) != 1 || args[0] != "list" {
		return output.NewError(output.CodeUsage, "usage: agentctl schema list", false)
	}
	schemas := []map[string]any{
		{"name": "adapter-manifest", "version": 1, "file": "schemas/adapter-manifest.schema.json"},
		{"name": "callback-envelope", "version": 1, "file": "schemas/callback-envelope.schema.json"},
		{"name": "config-bundle", "version": 1, "file": "schemas/config-bundle.schema.json"},
		{"name": "context-result", "version": 1, "file": "schemas/context-result.schema.json"},
		{"name": "data-cleanup-plan", "version": 1, "file": "schemas/data-cleanup-plan.schema.json"},
		{"name": "data-inventory", "version": 1, "file": "schemas/data-inventory.schema.json"},
		{"name": "error", "version": 1, "file": "schemas/error.schema.json"},
		{"name": "event", "version": 1, "file": "schemas/event.schema.json"},
		{"name": "event-page", "version": 1, "file": "schemas/event-page.schema.json"},
		{"name": "execution", "version": 1, "file": "schemas/execution.schema.json"},
		{"name": "fanout-manifest", "version": 1, "file": "schemas/fanout-manifest.schema.json"},
		{"name": "knowledge-source", "version": 1, "file": "schemas/knowledge-source.schema.json"},
		{"name": "outcome", "version": 1, "file": "schemas/outcome.schema.json"},
		{"name": "skill-pack", "version": 1, "file": "schemas/skill-pack.schema.json"},
		{"name": "skill-pack-report", "version": 1, "file": "schemas/skill-pack-report.schema.json"},
		{"name": "subscription", "version": 1, "file": "schemas/subscription.schema.json"},
	}
	lines := make([]output.Line, 0, len(schemas))
	for _, item := range schemas {
		lines = append(lines, output.Line{Lead: "schema", Fields: []output.Field{{Name: "name", Value: item["name"]}, {Name: "version", Value: item["version"]}, {Name: "file", Value: item["file"]}}})
	}
	if err := renderer.Success(output.Success{Result: map[string]any{"schemas": schemas}, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func (a *app) examplesCommand(renderer output.Renderer, args []string) *output.Error {
	if len(args) != 0 {
		return output.NewError(output.CodeUsage, "examples takes no arguments", false)
	}
	examples := [][]string{
		{"agentctl", "route", "explain", "--", "glm"},
		{"agentctl", "run", "--adapter", "codex", "--", "codex", "exec", "..."},
		{"agentctl", "run", "--prompt-file", "task.md", "--prompt-delivery", "argv", "--", "codex", "exec", "--json"},
		{"agentctl", "fanout", "--manifest", "fanout.json"},
		{"agentctl", "skills", "plan"},
		{"agentctl", "subscribe", "create", "--execution", "exec-...", "--kind", "terminal,attention", "--destination", "file", "--target", "/absolute/events.ndjson"},
		{"agentctl", "knowledge", "compile", "--source", "source.json=/checkout", "--output", "/bundle"},
		{"agentctl", "context", "--bundle", "/bundle", "--query", "deployment", "--render", "/context.md"},
	}
	lines := make([]output.Line, 0, len(examples))
	for _, argv := range examples {
		lines = append(lines, output.Line{Lead: "example", Fields: []output.Field{{Name: "argv", Value: argv}}})
	}
	if err := renderer.Success(output.Success{Result: map[string]any{"examples": examples}, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}
