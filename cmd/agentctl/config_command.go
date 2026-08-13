package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func (a *app) configCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "usage: agentctl config set-profile|show|validate|doctor|bundle|source ...", false)
	}
	path, err := configPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve config path", false, err)
	}
	switch args[0] {
	case "show", "validate", "doctor":
		if len(args) != 1 {
			return output.NewError(output.CodeUsage, "config "+args[0]+" takes no positional arguments", false)
		}
		resolution, err := config.Resolve(path, c.configBundle)
		if err != nil {
			return mapConfigError("read config", err)
		}
		cfg := resolution.Config
		if args[0] == "doctor" {
			report, err := config.Doctor(ctx, cfg, c.profile)
			report.Bundle = resolution.Bundle
			report.Composition = resolution.Composition
			if err != nil {
				if errors.Is(err, config.ErrProfileMissing) {
					return mapConfigError("resolve config doctor profile", err)
				}
				return output.Wrap(output.CodeDependencyUnavailable, "config provenance checks failed", true, err)
			}
			if c.configBundle == "" {
				source, sourceErr := config.SourceStatusReadOnly(ctx, path)
				if sourceErr != nil {
					return mapConfigError("read config source status", sourceErr)
				}
				report.Source = &source
				if source.Configured && !source.InSync {
					report.Valid = false
					report.Errors = append(report.Errors, "config source drift")
				}
			}
			lines := []output.Line{{Lead: "config.doctor", Fields: []output.Field{{Name: "valid", Value: report.Valid}, {Name: "profile", Value: report.Profile}, {Name: "checks", Value: len(report.Checks)}}}}
			if err := renderer.Success(output.Success{Result: report, Lines: lines}); err != nil {
				return output.Wrap(output.CodeInternal, "write output", false, err)
			}
			return nil
		}
		result := any(resolution)
		if c.profile != "" {
			name, profile, err := cfg.ResolveProfile(c.profile)
			if err != nil {
				return mapConfigError("resolve profile", err)
			}
			result = map[string]any{"name": name, "profile": profile, "base_path": resolution.BasePath, "base_present": resolution.BasePresent, "bundle": resolution.Bundle, "composition_order": resolution.Composition, "notes": resolution.Notes}
		}
		if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "config", Fields: []output.Field{{Name: "valid", Value: true}, {Name: "path", Value: path}, {Name: "profiles", Value: len(cfg.Profiles)}, {Name: "default", Value: cfg.DefaultProfile}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	case "bundle":
		return a.configBundleCommand(renderer, path, c, args[1:])
	case "source":
		if c.configBundle != "" {
			return output.NewError(output.CodeUsage, "config source cannot be combined with --config-bundle", false)
		}
		return a.configSourceCommand(ctx, renderer, path, args[1:])
	case "set-profile":
		if c.configBundle != "" {
			return output.NewError(output.CodeUsage, "set-profile cannot be combined with --config-bundle", false)
		}
		return a.configSetProfile(renderer, path, args[1:])
	default:
		return output.NewError(output.CodeUsage, "config requires set-profile, show, validate, doctor, bundle, or source", false)
	}
}

func (a *app) configSourceCommand(ctx context.Context, renderer output.Renderer, configPath string, args []string) *output.Error {
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "usage: agentctl config source init|status|update|restore ...", false)
	}
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return output.NewError(output.CodeUsage, "config source status takes no arguments", false)
		}
		status, err := config.SourceStatusReadOnly(ctx, configPath)
		if err != nil {
			return mapConfigError("read config source status", err)
		}
		if err := renderer.Success(output.Success{Result: status, Lines: []output.Line{{Lead: "config.source", Fields: []output.Field{{Name: "configured", Value: status.Configured}, {Name: "in_sync", Value: status.InSync}, {Name: "commit", Value: status.AppliedCommit}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	case "update":
		plan := false
		for _, arg := range args[1:] {
			if arg == "--plan" {
				plan = true
				continue
			}
			return output.NewError(output.CodeUsage, "unknown config source update flag", false).WithDetail("flag", arg)
		}
		if plan {
			statePath, err := config.SourceStatePath(configPath)
			if err != nil {
				return mapConfigError("resolve config source state", err)
			}
			state, err := config.LoadSourceState(statePath)
			if err != nil {
				return mapConfigError("read config source state", err)
			}
			proposal, err := config.PlanSource(configPath, state.Spec, "update")
			if err != nil {
				return mapConfigError("plan config source update", err)
			}
			if err := renderer.Success(output.Success{Result: proposal, Lines: []output.Line{{Lead: "config.source.plan", Fields: []output.Field{{Name: "operation", Value: proposal.Operation}, {Name: "plan_network", Value: proposal.PlanEffects.NetworkAccess}, {Name: "apply_network", Value: proposal.ApplyEffects.NetworkAccess}}}}}); err != nil {
				return output.Wrap(output.CodeInternal, "write output", false, err)
			}
			return nil
		}
		update, err := config.UpdateSource(ctx, configPath, a.currentTime())
		if err != nil {
			return mapConfigError("update config source", err)
		}
		if err := renderer.Success(output.Success{Result: update, Lines: []output.Line{{Lead: "config.source.update", Fields: []output.Field{{Name: "changed", Value: update.Changed}, {Name: "in_sync", Value: update.Status.InSync}, {Name: "commit", Value: update.Status.AppliedCommit}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	case "restore":
		plan := false
		for _, arg := range args[1:] {
			if arg == "--plan" {
				plan = true
				continue
			}
			return output.NewError(output.CodeUsage, "unknown config source restore flag", false).WithDetail("flag", arg)
		}
		if plan {
			status, err := config.SourceStatusReadOnly(ctx, configPath)
			if err != nil {
				return mapConfigError("plan config source restore", err)
			}
			wouldRestore, blockers := sourceRestorePlan(status)
			result := map[string]any{"operation": "restore", "network_access": false, "mutates": false, "would_restore": wouldRestore, "blocked_by": blockers, "status": status}
			if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "config.source.restore.plan", Fields: []output.Field{{Name: "would_restore", Value: wouldRestore}, {Name: "network", Value: false}, {Name: "mutates", Value: false}}}}}); err != nil {
				return output.Wrap(output.CodeInternal, "write output", false, err)
			}
			return nil
		}
		update, err := config.RestoreSource(ctx, configPath)
		if err != nil {
			return mapConfigError("restore config source", err)
		}
		if err := renderer.Success(output.Success{Result: update, Lines: []output.Line{{Lead: "config.source.restore", Fields: []output.Field{{Name: "changed", Value: update.Changed}, {Name: "in_sync", Value: update.Status.InSync}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	case "init":
		var remote, ref, bundlePath, checkoutPath string
		plan := false
		for i := 1; i < len(args); i++ {
			take := func() (string, *output.Error) {
				if i+1 >= len(args) {
					return "", output.NewError(output.CodeUsage, args[i]+" requires a value", false)
				}
				i++
				return args[i], nil
			}
			switch args[i] {
			case "--remote":
				value, problem := take()
				if problem != nil {
					return problem
				}
				remote = value
			case "--ref":
				value, problem := take()
				if problem != nil {
					return problem
				}
				ref = value
			case "--bundle":
				value, problem := take()
				if problem != nil {
					return problem
				}
				bundlePath = value
			case "--checkout":
				value, problem := take()
				if problem != nil {
					return problem
				}
				checkoutPath = value
			case "--plan":
				plan = true
			default:
				return output.NewError(output.CodeUsage, "unknown config source init flag", false).WithDetail("flag", args[i])
			}
		}
		if strings.TrimSpace(remote) == "" {
			return output.NewError(output.CodeUsage, "config source init requires --remote", false)
		}
		spec := config.SourceSpec{Remote: remote, Ref: ref, BundlePath: bundlePath, CheckoutPath: checkoutPath}
		if plan {
			proposal, err := config.PlanSource(configPath, spec, "init")
			if err != nil {
				return mapConfigError("plan config source init", err)
			}
			if err := renderer.Success(output.Success{Result: proposal, Lines: []output.Line{{Lead: "config.source.plan", Fields: []output.Field{{Name: "operation", Value: proposal.Operation}, {Name: "plan_network", Value: proposal.PlanEffects.NetworkAccess}, {Name: "apply_network", Value: proposal.ApplyEffects.NetworkAccess}}}}}); err != nil {
				return output.Wrap(output.CodeInternal, "write output", false, err)
			}
			return nil
		}
		update, err := config.InitSource(ctx, configPath, spec, a.currentTime())
		if err != nil {
			return mapConfigError("initialize config source", err)
		}
		if err := renderer.Success(output.Success{Result: update, Lines: []output.Line{{Lead: "config.source.init", Fields: []output.Field{{Name: "in_sync", Value: update.Status.InSync}, {Name: "commit", Value: update.Status.AppliedCommit}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	default:
		return output.NewError(output.CodeUsage, "config source requires init, status, update, or restore", false)
	}
}

func sourceRestorePlan(status config.SourceStatus) (bool, []string) {
	if !status.Configured {
		return false, nil
	}
	restorable := false
	blockers := []string{}
	for _, drift := range status.Drift {
		switch drift {
		case "live_config", "live_config_missing_or_unreadable", "live_config_unmanaged_or_invalid":
			restorable = true
		default:
			blockers = append(blockers, drift)
		}
	}
	return restorable && len(blockers) == 0, blockers
}

func (a *app) currentTime() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *app) configBundleCommand(renderer output.Renderer, basePath string, c common, args []string) *output.Error {
	if len(args) != 1 || args[0] != "validate" && args[0] != "show" && args[0] != "plan" {
		return output.NewError(output.CodeUsage, "usage: agentctl --config-bundle <local-path> config bundle validate|show|plan", false)
	}
	if strings.TrimSpace(c.configBundle) == "" {
		return output.NewError(output.CodeUsage, "config bundle requires explicit --config-bundle <local-path>", false)
	}
	if args[0] == "plan" {
		plan, err := config.PlanBundle(basePath, c.configBundle)
		if err != nil {
			return mapConfigError("plan config bundle", err)
		}
		if err := renderer.Success(output.Success{Result: plan, Lines: []output.Line{{Lead: "config.bundle.plan", Fields: []output.Field{{Name: "valid", Value: plan.Valid}, {Name: "digest", Value: plan.Provenance.SHA256}, {Name: "profiles", Value: len(plan.AddProfiles)}, {Name: "mutates", Value: plan.Mutates}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	}
	bundle, provenance, err := config.LoadBundle(c.configBundle)
	if err != nil {
		return mapConfigError("read config bundle", err)
	}
	result := map[string]any{"valid": true, "provenance": provenance}
	if args[0] == "show" {
		result["bundle"] = bundle
	} else {
		resolution, err := config.Resolve(basePath, c.configBundle)
		if err != nil {
			return mapConfigError("validate config bundle composition", err)
		}
		result["base_path"] = resolution.BasePath
		result["base_present"] = resolution.BasePresent
		result["composition_order"] = resolution.Composition
		result["notes"] = resolution.Notes
	}
	if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "config.bundle", Fields: []output.Field{{Name: "valid", Value: true}, {Name: "path", Value: provenance.SourcePath}, {Name: "digest", Value: provenance.SHA256}, {Name: "profiles", Value: len(bundle.Profiles)}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func (a *app) configSetProfile(renderer output.Renderer, path string, args []string) *output.Error {
	var name string
	adapters := map[string]config.Adapter{}
	var multica config.Multica
	setDefault, replace := false, false
	for i := 0; i < len(args); i++ {
		take := func() (string, *output.Error) {
			if i+1 >= len(args) {
				return "", output.NewError(output.CodeUsage, args[i]+" requires a value", false)
			}
			i++
			return args[i], nil
		}
		switch args[i] {
		case "--name":
			value, problem := take()
			if problem != nil {
				return problem
			}
			name = strings.TrimSpace(value)
		case "--adapter":
			value, problem := take()
			if problem != nil {
				return problem
			}
			adapterName, executable, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(adapterName) == "" || strings.TrimSpace(executable) == "" {
				return output.NewError(output.CodeUsage, "--adapter must be NAME=EXECUTABLE", false)
			}
			adapters[strings.TrimSpace(adapterName)] = config.Adapter{Executable: strings.TrimSpace(executable)}
		case "--multica-executable":
			value, problem := take()
			if problem != nil {
				return problem
			}
			multica.Executable = value
		case "--multica-profile":
			value, problem := take()
			if problem != nil {
				return problem
			}
			multica.Profile = value
		case "--workspace-id":
			value, problem := take()
			if problem != nil {
				return problem
			}
			multica.WorkspaceID = value
		case "--server-url":
			value, problem := take()
			if problem != nil {
				return problem
			}
			multica.ServerURL = value
		case "--app-url":
			value, problem := take()
			if problem != nil {
				return problem
			}
			multica.AppURL = value
		case "--default":
			setDefault = true
		case "--replace":
			replace = true
		default:
			return output.NewError(output.CodeUsage, "unknown config set-profile flag", false).WithDetail("flag", args[i])
		}
	}
	if name == "" {
		return output.NewError(output.CodeUsage, "--name is required", false)
	}
	input := config.ProfileInput{Adapters: adapters}
	if multica.Executable != "" || multica.Profile != "" || multica.WorkspaceID != "" || multica.ServerURL != "" || multica.AppURL != "" {
		input.Multica = &multica
	}
	profile, err := config.BuildProfile(input)
	if err != nil {
		return output.Wrap(output.CodeUsage, "invalid profile", false, err)
	}
	cfg, err := config.Load(path)
	exists := err == nil
	if errors.Is(err, config.ErrNotFound) {
		cfg = config.Config{SchemaVersion: config.SchemaVersion, Profiles: map[string]config.Profile{}}
	} else if err != nil {
		return mapConfigError("read existing config", err)
	}
	// set-profile cannot author or clear advisory preferences. Preserve any
	// reviewed preferences already attached to the named profile, including
	// when --replace updates the adapter/Multica fields that this command owns.
	if existing, ok := cfg.Profiles[name]; ok && existing.AgentPreferences != nil {
		preferences := *existing.AgentPreferences
		preferences.Preferred = append([]config.AgentPreference(nil), existing.AgentPreferences.Preferred...)
		preferences.Notes = append([]string(nil), existing.AgentPreferences.Notes...)
		profile.AgentPreferences = &preferences
	}
	if replace {
		cfg, err = config.UpsertProfile(cfg, name, profile, true)
	} else {
		cfg, err = config.MergeProfile(cfg, name, profile)
	}
	if err != nil {
		return mapConfigError("set profile", err)
	}
	if setDefault || cfg.DefaultProfile == "" {
		cfg.DefaultProfile = name
	}
	if err := config.Save(path, cfg, exists); err != nil {
		return mapConfigError("write config", err)
	}
	if err := renderer.Success(output.Success{Result: map[string]any{"path": path, "name": name, "profile": profile, "default_profile": cfg.DefaultProfile}, Lines: []output.Line{{Lead: "config.profile", Fields: []output.Field{{Name: "name", Value: name}, {Name: "default", Value: cfg.DefaultProfile == name}, {Name: "adapters", Value: len(profile.Adapters)}, {Name: "multica", Value: profile.Multica != nil}, {Name: "agent_preferences", Value: profile.AgentPreferences != nil}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func configPath(c common) (string, error) {
	if c.configPath != "" {
		return c.configPath, nil
	}
	return config.DefaultPath()
}

func configResolution(c common) (config.Resolution, error) {
	path, err := configPath(c)
	if err != nil {
		return config.Resolution{}, err
	}
	return config.Resolve(path, c.configBundle)
}

func mapConfigError(message string, err error) *output.Error {
	switch {
	case errors.Is(err, config.ErrNotFound), errors.Is(err, config.ErrProfileMissing):
		return output.Wrap(output.CodeNotFound, message, false, err)
	case errors.Is(err, config.ErrConflict):
		return output.Wrap(output.CodeConflict, message, false, err)
	case errors.Is(err, config.ErrUnsafePath), errors.Is(err, config.ErrUnmanaged):
		return output.Wrap(output.CodeAuthorizationDenied, message, false, err)
	case errors.Is(err, config.ErrSourceAuth):
		return output.Wrap(output.CodeAuthorizationDenied, message, false, err)
	case errors.Is(err, config.ErrSourceGit):
		return output.Wrap(output.CodeDependencyUnavailable, message, true, err)
	default:
		return output.Wrap(output.CodeUsage, message, false, err)
	}
}
