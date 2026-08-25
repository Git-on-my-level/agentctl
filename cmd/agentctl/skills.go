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
		return output.NewError(output.CodeUsage, "usage: agentctl skills plan|status|doctor|update|reconcile|diff|restore|propose ...", false)
	}
	command := args[0]
	if command != "plan" && command != "status" && command != "doctor" && command != "update" && command != "reconcile" && command != "diff" && command != "restore" && command != "propose" {
		return output.NewError(output.CodeUsage, "skills requires plan, status, doctor, update, reconcile, diff, restore, or propose", false)
	}
	if c.configBundle != "" {
		return output.NewError(output.CodeUsage, "skills requires the live reviewed config and cannot use an invocation-scoped config bundle", false)
	}
	home, harnessValue, proposalRoot, planOnly, apply := "", "", "", false, false
	skillName, start := "", 1
	if command == "diff" || command == "restore" || command == "propose" {
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return output.NewError(output.CodeUsage, "skills "+command+" requires a skill name", false)
		}
		skillName, start = args[1], 2
	}
	for i := start; i < len(args); i++ {
		if args[i] == "--plan" && command == "update" {
			planOnly = true
			continue
		}
		if args[i] == "--apply" && (command == "restore" || command == "propose") {
			apply = true
			continue
		}
		if args[i] != "--home" && args[i] != "--harness" && !(args[i] == "--worktree-root" && command == "propose") {
			return output.NewError(output.CodeUsage, "unknown skills flag", false).WithDetail("flag", args[i])
		}
		if i+1 >= len(args) {
			return output.NewError(output.CodeUsage, args[i]+" requires a value", false)
		}
		i++
		switch args[i-1] {
		case "--home":
			home = args[i]
		case "--harness":
			harnessValue = args[i]
		case "--worktree-root":
			proposalRoot = args[i]
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
	cfg, err := config.Load(path)
	if err != nil {
		return mapConfigError("load skill selection", err)
	}
	if cfg.Skills == nil {
		return output.NewError(output.CodeCapabilityUnavailable, "config does not select a Skill Hub pack", false).
			WithActions(output.NextAction{Label: "Add a reviewed Skill Hub selection to config", Argv: []string{"agentctl", "help", "skills"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	selection := skillpack.HubSelection{Remote: cfg.Skills.Source.Remote, Ref: cfg.Skills.Source.Ref, ManifestPath: cfg.Skills.Source.ManifestPath}
	hubStatus, err := skillpack.HubSourceStatusReadOnly(ctx, home, selection)
	if err != nil {
		return mapSkillpackError("inspect Skill Hub source", skillpack.Report{}, err)
	}
	if command == "update" && planOnly {
		result := map[string]any{"operation": "update", "source": selection, "configured": hubStatus.Configured, "in_sync": hubStatus.InSync, "network_access": false, "apply_network_access": true, "apply_mutates": true, "update_policy": cfg.Skills.UpdatePolicy}
		return renderSkillResult(renderer, "skills.update.plan", result, nil, []output.Field{{Name: "configured", Value: hubStatus.Configured}, {Name: "in_sync", Value: hubStatus.InSync}, {Name: "policy", Value: cfg.Skills.UpdatePolicy}})
	}
	if command == "update" {
		updated, err := skillpack.UpdateHubSource(ctx, home, selection, a.currentTime())
		if err != nil {
			return mapSkillpackError("update Skill Hub source", skillpack.Report{}, err)
		}
		hubStatus = updated.Status
	}
	if !hubStatus.Configured {
		return output.NewError(output.CodeCapabilityUnavailable, "Skill Hub source is not initialized on this host", false).
			WithActions(output.NextAction{Label: "Fetch and reconcile the selected Skill Hub pack", Argv: []string{"agentctl", "skills", "update"}, Mutates: true, SideEffectClass: output.ExternalSideEffect, Preconditions: []string{"reviewed non-secret Skill Hub selection"}})
	}
	if !hubStatus.InSync {
		return output.NewError(output.CodeConflict, "skills refuse a drifted Skill Hub checkout", false).WithDetail("drift", hubStatus.Drift)
	}
	if proposalRoot == "" {
		proposalRoot = strings.TrimSpace(a.getenv("AGENTCTL_SKILL_PROPOSAL_ROOT"))
	}
	options := skillpack.Options{Source: skillpack.SourceFromHubStatus(hubStatus), Home: home, DetectedHarnesses: detected, ProposalRoot: proposalRoot}
	if command == "diff" {
		review, err := skillpack.ReviewDrift(options, skillName, harnessValue)
		if err != nil {
			return mapSkillpackError("review managed skill drift", skillpack.Report{}, err)
		}
		actions := []output.NextAction{{Label: "Prepare a Skill Hub proposal", Argv: []string{"agentctl", "skills", "propose", skillName}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"review the changed-file inventory"}}, {Label: "Plan restoration from Skill Hub", Argv: []string{"agentctl", "skills", "restore", skillName}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"confirm the local edits are obsolete"}}}
		return renderSkillResult(renderer, "skills.diff", review, actions, []output.Field{{Name: "skill", Value: skillName}, {Name: "destination", Value: review.Destination}, {Name: "files", Value: len(review.Files)}})
	}
	if command == "restore" {
		result, err := skillpack.RestoreDrift(options, skillName, harnessValue, apply, a.currentTime())
		if err != nil {
			return mapSkillpackError("restore managed skill drift", skillpack.Report{}, err)
		}
		actions := []output.NextAction{}
		if !apply {
			argv := []string{"agentctl", "skills", "restore", skillName, "--apply"}
			if harnessValue != "" {
				argv = append(argv, "--harness", harnessValue)
			}
			actions = append(actions, output.NextAction{Label: "Back up and restore the Skill Hub version", Argv: argv, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{"reviewed local drift is obsolete"}})
		}
		return renderSkillResult(renderer, "skills.restore", result, actions, []output.Field{{Name: "skill", Value: skillName}, {Name: "applied", Value: result.Applied}, {Name: "backup", Value: result.BackupPath}})
	}
	if command == "propose" {
		result, err := skillpack.ProposeDrift(ctx, config.NativeSourceGit{}, options, skillName, harnessValue, apply, a.currentTime())
		if err != nil {
			return mapSkillpackError("prepare Skill Hub proposal", skillpack.Report{}, err)
		}
		actions := []output.NextAction{}
		if !apply {
			argv := []string{"agentctl", "skills", "propose", skillName, "--apply"}
			if harnessValue != "" {
				argv = append(argv, "--harness", harnessValue)
			}
			actions = append(actions, output.NextAction{Label: "Create a local review branch and worktree", Argv: argv, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{"local edits should be proposed to Skill Hub", "publishing remains a separate Git or PR action"}})
		}
		return renderSkillResult(renderer, "skills.propose", result, actions, []output.Field{{Name: "skill", Value: skillName}, {Name: "applied", Value: result.Applied}, {Name: "branch", Value: result.Branch}, {Name: "worktree", Value: result.WorktreePath}})
	}
	var report skillpack.Report
	if command == "reconcile" {
		report, err = skillpack.Reconcile(options)
	} else if command == "update" {
		report, err = skillpack.ReconcileAutoClean(options)
	} else {
		report, err = skillpack.Plan(options)
	}
	if err != nil {
		return mapSkillpackError("evaluate managed Skill Hub pack", report, err)
	}
	lines := skillReportLines("skills."+command, report)
	actions := []output.NextAction{}
	if command != "reconcile" && command != "update" && report.Healthy && report.Changed > 0 {
		argv := []string{"agentctl", "skills", "reconcile"}
		if harnessValue != "" {
			argv = append(argv, "--harness", strings.TrimSpace(harnessValue))
		}
		actions = append(actions, output.NextAction{Label: "Reconcile pinned Skill Hub skills", Argv: argv, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{"review the plan actions"}})
	}
	for _, action := range report.Actions {
		if action.State != "drifted" && action.State != "conflict" {
			continue
		}
		argv := []string{"agentctl", "skills", "diff", action.Skill}
		if len(action.Harnesses) == 1 {
			argv = append(argv, "--harness", action.Harnesses[0])
		}
		actions = append(actions, output.NextAction{Label: "Review preserved local skill " + action.Skill, Argv: argv, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"decide whether to restore or propose the local edits"}})
	}
	if err := renderer.Success(output.Success{Result: map[string]any{"source": hubStatus, "update_policy": cfg.Skills.UpdatePolicy, "report": report}, Lines: lines, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func skillReportLines(lead string, report skillpack.Report) []output.Line {
	lines := []output.Line{{Lead: lead, Fields: []output.Field{{Name: "healthy", Value: report.Healthy}, {Name: "commit", Value: report.Source.Commit}, {Name: "changed", Value: report.Changed}, {Name: "applied", Value: report.Applied}, {Name: "conflicts", Value: report.Conflicts}, {Name: "unsupported", Value: report.Unsupported}}}}
	for _, action := range report.Actions {
		lines = append(lines, output.Line{Lead: "skill", Fields: []output.Field{{Name: "name", Value: action.Skill}, {Name: "harnesses", Value: action.Harnesses}, {Name: "state", Value: action.State}, {Name: "destination", Value: action.Destination}, {Name: "changed", Value: action.Changed}}})
	}
	return lines
}

func renderSkillResult(renderer output.Renderer, lead string, result any, actions []output.NextAction, fields []output.Field) *output.Error {
	if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: lead, Fields: fields}}, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func mapSkillpackError(operation string, report skillpack.Report, err error) *output.Error {
	code := output.CodeUsage
	if errors.Is(err, skillpack.ErrConflict) || errors.Is(err, config.ErrConflict) {
		code = output.CodeConflict
	} else if errors.Is(err, skillpack.ErrUnsupportedSchema) {
		code = output.CodeUnsupportedSchema
	} else if errors.Is(err, os.ErrNotExist) {
		code = output.CodeNotFound
	} else if errors.Is(err, os.ErrPermission) {
		code = output.CodeAuthorizationDenied
	} else if errors.Is(err, config.ErrSourceAuth) {
		code = output.CodeAuthorizationDenied
	} else if errors.Is(err, config.ErrSourceGit) {
		code = output.CodeDependencyUnavailable
	} else if !errors.Is(err, skillpack.ErrInvalidInput) {
		code = output.CodeInternal
	}
	problem := output.Wrap(code, operation, false, err)
	if report.SchemaVersion != 0 {
		problem.WithDetail("report", report)
	}
	return problem
}
