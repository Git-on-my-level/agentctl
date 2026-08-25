package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/skillpack"
	"github.com/Git-on-my-level/agentctl/internal/updatecheck"
)

func startUpdateWorker() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "_update-worker")
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	command.Env = os.Environ()
	prepareDetachedCommand(command)
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func (a *app) updateWorker(parent context.Context) int {
	statePath, policyPath, err := updatecheck.DefaultPaths(a.getenv)
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	mode, modeErr := updatecheck.ResolveMode(policyPath, a.getenv)
	if modeErr == nil && mode == updatecheck.ModeAuto {
		_, _ = updatecheck.Apply(ctx, updatecheck.ApplyOptions{Check: updatecheck.Options{CurrentVersion: version, StatePath: statePath, Getenv: a.getenv}})
	}
	_, _ = a.updateSkillsAutoClean(ctx, common{}, false)
	return 0
}

func (a *app) updateCommand(ctx context.Context, renderer output.Renderer, args []string) *output.Error {
	statePath, policyPath, err := updatecheck.DefaultPaths(a.getenv)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve update state", false, err)
	}
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "usage: agentctl update status|now|policy auto|notify|off", false)
	}
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return output.NewError(output.CodeUsage, "usage: agentctl update status", false)
		}
		status, err := updatecheck.ReadStatus(statePath, policyPath, a.getenv)
		if err != nil {
			return output.Wrap(output.CodeInternal, "read update status", false, err)
		}
		skills := a.skillsUpdateStatus(ctx, common{})
		_ = renderer.Success(output.Success{Result: map[string]any{"binary": status, "skills": skills}, Lines: []output.Line{{Lead: "update.binary", Fields: []output.Field{{Name: "mode", Value: status.Mode}, {Name: "checked_on", Value: status.CheckedOn}, {Name: "latest_version", Value: status.LatestVersion}, {Name: "installed_version", Value: status.InstalledVersion}}}, {Lead: "update.skills", Fields: []output.Field{{Name: "policy", Value: skills["policy"]}, {Name: "configured", Value: skills["configured"]}, {Name: "checked_on", Value: skills["checked_on"]}, {Name: "healthy", Value: skills["healthy"]}}}}})
		return nil
	case "policy":
		if len(args) != 2 {
			return output.NewError(output.CodeUsage, "usage: agentctl update policy auto|notify|off", false)
		}
		mode := updatecheck.Mode(args[1])
		if err := updatecheck.SetMode(policyPath, mode); err != nil {
			return output.Wrap(output.CodeUsage, "set update policy", false, err)
		}
		_ = renderer.Success(output.Success{Result: map[string]any{"mode": mode, "policy_path": policyPath}, Lines: []output.Line{{Lead: "update", Fields: []output.Field{{Name: "mode", Value: mode}, {Name: "policy_path", Value: policyPath}}}}})
		return nil
	case "now":
		if len(args) != 1 {
			return output.NewError(output.CodeUsage, "usage: agentctl update now", false)
		}
		result, err := updatecheck.Apply(ctx, updatecheck.ApplyOptions{Check: updatecheck.Options{CurrentVersion: version, StatePath: statePath, Getenv: a.getenv, Force: true}})
		if err != nil {
			var applyError *updatecheck.ApplyError
			if errors.As(err, &applyError) {
				return output.Wrap(output.CodeConflict, "apply agentctl update", applyError.Retryable, err).WithDetail("update_error_code", applyError.Code)
			}
			return output.Wrap(output.CodeInternal, "apply agentctl update", false, err)
		}
		skills, skillsErr := a.updateSkillsAutoClean(ctx, common{}, true)
		if skillsErr != nil {
			return mapSkillpackError("apply Skill Hub update", skills, skillsErr)
		}
		_ = renderer.Success(output.Success{Result: map[string]any{"binary": result, "skills": skills}, Lines: []output.Line{{Lead: "update.binary", Fields: []output.Field{{Name: "current_version", Value: result.CurrentVersion}, {Name: "installed_version", Value: result.InstalledVersion}, {Name: "updated", Value: result.Updated}}}, {Lead: "update.skills", Fields: []output.Field{{Name: "healthy", Value: skills.Healthy}, {Name: "applied", Value: skills.Applied}, {Name: "conflicts", Value: skills.Conflicts}}}}})
		return nil
	default:
		return output.NewError(output.CodeUsage, "usage: agentctl update status|now|policy auto|notify|off", false)
	}
}

func (a *app) skillsUpdateStatus(ctx context.Context, c common) map[string]any {
	result := map[string]any{"configured": false, "policy": "unconfigured", "healthy": false}
	path, err := configPath(c)
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Skills == nil {
		if err != nil {
			result["error"] = err.Error()
		}
		return result
	}
	result["policy"] = cfg.Skills.UpdatePolicy
	home, err := os.UserHomeDir()
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	selection := skillpack.HubSelection{Remote: cfg.Skills.Source.Remote, Ref: cfg.Skills.Source.Ref, ManifestPath: cfg.Skills.Source.ManifestPath}
	status, err := skillpack.HubSourceStatusReadOnly(ctx, home, selection)
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	result["configured"], result["healthy"], result["checked_on"], result["commit"], result["drift"] = status.Configured, status.InSync, status.CheckedOn, status.AppliedCommit, status.Drift
	if status.InSync && status.Configured {
		detected, problem := parseBootstrapHarnesses(home, "", "", a.getenv)
		if problem != nil {
			result["healthy"], result["error"] = false, problem.Message
			return result
		}
		report, planErr := skillpack.Plan(skillpack.Options{Source: skillpack.SourceFromHubStatus(status), Home: home, DetectedHarnesses: detected})
		if planErr != nil {
			result["healthy"], result["error"] = false, planErr.Error()
			return result
		}
		result["healthy"], result["conflicts"], result["changed"], result["unsupported"] = report.Healthy, report.Conflicts, report.Changed, report.Unsupported
	}
	return result
}

func (a *app) skillsAutoDue(c common) bool {
	path, err := configPath(c)
	if err != nil {
		return false
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Skills == nil || cfg.Skills.UpdatePolicy != "auto-clean" {
		return false
	}
	home, err := os.UserHomeDir()
	return err == nil && skillpack.HubUpdateDue(home, a.currentTime())
}

func (a *app) updateSkillsAutoClean(ctx context.Context, c common, force bool) (skillpack.Report, error) {
	path, err := configPath(c)
	if err != nil {
		return skillpack.Report{}, err
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Skills == nil || cfg.Skills.UpdatePolicy != "auto-clean" {
		return skillpack.Report{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return skillpack.Report{}, err
	}
	if !force && !skillpack.HubUpdateDue(home, a.currentTime()) {
		return skillpack.Report{}, nil
	}
	detected, problem := parseBootstrapHarnesses(home, "", "", a.getenv)
	if problem != nil {
		return skillpack.Report{}, fmt.Errorf("detect skill harnesses: %s", problem.Message)
	}
	selection := skillpack.HubSelection{Remote: cfg.Skills.Source.Remote, Ref: cfg.Skills.Source.Ref, ManifestPath: cfg.Skills.Source.ManifestPath}
	updated, err := skillpack.UpdateHubSource(ctx, home, selection, a.currentTime())
	if err != nil {
		return skillpack.Report{}, err
	}
	return skillpack.ReconcileAutoClean(skillpack.Options{Source: skillpack.SourceFromHubStatus(updated.Status), Home: home, DetectedHarnesses: detected})
}
