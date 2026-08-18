package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/output"
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
	mode, err := updatecheck.ResolveMode(policyPath, a.getenv)
	if err != nil || mode != updatecheck.ModeAuto {
		return 0
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	_, _ = updatecheck.Apply(ctx, updatecheck.ApplyOptions{Check: updatecheck.Options{CurrentVersion: version, StatePath: statePath, Getenv: a.getenv}})
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
		_ = renderer.Success(output.Success{Result: status, Lines: []output.Line{{Lead: "update", Fields: []output.Field{{Name: "mode", Value: status.Mode}, {Name: "checked_on", Value: status.CheckedOn}, {Name: "latest_version", Value: status.LatestVersion}, {Name: "installed_version", Value: status.InstalledVersion}}}}})
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
		_ = renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "update", Fields: []output.Field{{Name: "current_version", Value: result.CurrentVersion}, {Name: "installed_version", Value: result.InstalledVersion}, {Name: "updated", Value: result.Updated}}}}})
		return nil
	default:
		return output.NewError(output.CodeUsage, "usage: agentctl update status|now|policy auto|notify|off", false)
	}
}
