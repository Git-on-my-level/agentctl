package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/skillpack"
)

func (a *app) skillsCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "usage: agentctl skills plan|status|doctor|reconcile [--manifest path] [--home path] [--harness names]", false)
	}
	command := args[0]
	if command != "plan" && command != "status" && command != "doctor" && command != "reconcile" {
		return output.NewError(output.CodeUsage, "skills requires plan, status, doctor, or reconcile", false)
	}
	if c.configBundle != "" {
		return output.NewError(output.CodeUsage, "skills requires a configured Git source and cannot use an invocation-scoped config bundle", false)
	}
	manifestPath, home, harnessValue := skillpack.DefaultManifest, "", ""
	for i := 1; i < len(args); i++ {
		if args[i] != "--manifest" && args[i] != "--home" && args[i] != "--harness" {
			return output.NewError(output.CodeUsage, "unknown skills flag", false).WithDetail("flag", args[i])
		}
		if i+1 >= len(args) {
			return output.NewError(output.CodeUsage, args[i]+" requires a value", false)
		}
		i++
		switch args[i-1] {
		case "--manifest":
			manifestPath = args[i]
		case "--home":
			home = args[i]
		case "--harness":
			harnessValue = args[i]
		}
	}
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return output.Wrap(output.CodeDependencyUnavailable, "resolve home directory", false, err)
		}
	}
	if !filepath.IsAbs(home) {
		return output.NewError(output.CodeUsage, "skills home must be an absolute path", false).WithDetail("home", home)
	}
	home, err := filepath.Abs(filepath.Clean(home))
	if err != nil {
		return output.Wrap(output.CodeUsage, "resolve skills home", false, err)
	}
	detected, problem := parseBootstrapHarnesses(home, harnessValue, "", a.getenv)
	if problem != nil {
		return problem
	}
	path, err := configPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve config path", false, err)
	}
	status, err := config.SourceStatusReadOnly(ctx, path)
	if err != nil {
		return mapConfigError("read config source for skills", err)
	}
	if !status.Configured {
		return output.NewError(output.CodeCapabilityUnavailable, "skills require an initialized Git config source", false).
			WithActions(output.NextAction{Label: "Plan config source initialization", Argv: []string{"agentctl", "config", "source", "init", "--remote", "<git-remote>", "--plan"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"reviewed non-secret Git source"}})
	}
	if !status.InSync {
		return output.NewError(output.CodeConflict, "skills refuse a drifted config-source checkout", false).WithDetail("drift", status.Drift).
			WithActions(output.NextAction{Label: "Inspect config source drift", Argv: []string{"agentctl", "config", "source", "status"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	options := skillpack.Options{Source: skillpack.Source{RepositoryRoot: status.Source.CheckoutPath, Remote: status.Source.Remote, Commit: status.AppliedCommit, ManifestPath: manifestPath}, Home: home, DetectedHarnesses: detected}
	var report skillpack.Report
	if command == "reconcile" {
		report, err = skillpack.Reconcile(options)
	} else {
		report, err = skillpack.Plan(options)
	}
	if err != nil {
		code := output.CodeUsage
		if errors.Is(err, skillpack.ErrConflict) {
			code = output.CodeConflict
		} else if errors.Is(err, skillpack.ErrUnsupportedSchema) {
			code = output.CodeUnsupportedSchema
		} else if errors.Is(err, os.ErrNotExist) {
			code = output.CodeNotFound
		} else if errors.Is(err, os.ErrPermission) {
			code = output.CodeAuthorizationDenied
		} else if !errors.Is(err, skillpack.ErrInvalidInput) {
			code = output.CodeInternal
		}
		problem := output.Wrap(code, "evaluate managed skill pack", false, err)
		if report.SchemaVersion != 0 {
			problem.WithDetail("report", report)
		}
		return problem
	}
	lines := []output.Line{{Lead: "skills." + command, Fields: []output.Field{{Name: "healthy", Value: report.Healthy}, {Name: "commit", Value: report.Source.Commit}, {Name: "changed", Value: report.Changed}, {Name: "applied", Value: report.Applied}, {Name: "conflicts", Value: report.Conflicts}, {Name: "unsupported", Value: report.Unsupported}}}}
	for _, action := range report.Actions {
		lines = append(lines, output.Line{Lead: "skill", Fields: []output.Field{{Name: "name", Value: action.Skill}, {Name: "harnesses", Value: action.Harnesses}, {Name: "state", Value: action.State}, {Name: "destination", Value: action.Destination}, {Name: "changed", Value: action.Changed}}})
	}
	actions := []output.NextAction{}
	if command != "reconcile" && report.Healthy && report.Changed > 0 {
		argv := []string{"agentctl", "skills", "reconcile"}
		if manifestPath != skillpack.DefaultManifest {
			argv = append(argv, "--manifest", manifestPath)
		}
		if harnessValue != "" {
			argv = append(argv, "--harness", strings.TrimSpace(harnessValue))
		}
		actions = append(actions, output.NextAction{Label: "Reconcile managed skills", Argv: argv, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{"review the plan actions"}})
	}
	if err := renderer.Success(output.Success{Result: report, Lines: lines, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}
