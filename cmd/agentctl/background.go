package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const backgroundStartupTimeout = 30 * time.Second

func (a *app) runNativeBackground(ctx context.Context, renderer output.Renderer, c common, args []string, opts runOptions) *output.Error {
	journalPath, err := a.journalPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve background journal", false, err)
	}
	executionID := opts.executionID
	if executionID.IsZero() {
		var err error
		executionID, err = ids.NewExecutionID(ids.CryptoGenerator{})
		if err != nil {
			return output.Wrap(output.CodeInternal, "allocate background execution ID", false, err)
		}
	} else if _, found, problem := readBackgroundExecution(context.Background(), journalPath, executionID); problem != nil {
		return problem
	} else if found {
		return output.NewError(output.CodeConflict, "background execution ID already exists", false).WithDetail("execution_id", executionID.String())
	}
	executable, err := os.Executable()
	if err != nil {
		return output.Wrap(output.CodeDependencyUnavailable, "resolve agentctl executable for background worker", false, err)
	}
	workerArgs := backgroundCommandArgs(c, args, executionID, opts.executionID.IsZero())
	cmd := exec.Command(executable, workerArgs...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = os.Environ()
	prepareDetachedCommand(cmd)
	if err := cmd.Start(); err != nil {
		return output.Wrap(output.CodeDependencyUnavailable, "start background agentctl worker", true, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(backgroundStartupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if execution, found, problem := readBackgroundExecution(context.Background(), journalPath, executionID); problem != nil {
			return problem
		} else if found {
			return writeExecution(renderer, execution, "background")
		}
		select {
		case waitErr := <-done:
			if execution, found, problem := readBackgroundExecution(context.Background(), journalPath, executionID); problem != nil {
				return problem
			} else if found {
				return writeExecution(renderer, execution, "background")
			}
			detail := "worker exited before creating the execution"
			if waitErr != nil {
				detail = waitErr.Error()
			}
			return output.NewError(output.CodeExecutionFailed, "background agentctl worker failed during startup", false).WithDetail("execution_id", executionID.String()).WithDetail("worker_error", detail)
		case <-ctx.Done():
			return output.Wrap(output.CodeExecutionCancelled, "background launch observation cancelled; the detached worker may continue", false, ctx.Err()).WithDetail("execution_id", executionID.String()).WithDetail("worker_continues", true)
		case <-deadline.C:
			return output.NewError(output.CodeTimeout, "background worker did not acknowledge durable startup before the deadline and may continue", true).WithDetail("execution_id", executionID.String()).WithDetail("timeout", backgroundStartupTimeout.String()).WithDetail("worker_continues", true)
		case <-ticker.C:
		}
	}
}

func backgroundCommandArgs(c common, runArgs []string, executionID ids.ExecutionID, addExecutionID bool) []string {
	args := []string{"--output", "json"}
	appendFlag := func(name, value string) {
		if value != "" {
			args = append(args, name, value)
		}
	}
	appendFlag("--profile", c.profile)
	appendFlag("--context-file", c.contextFile)
	appendFlag("--config", c.configPath)
	appendFlag("--config-bundle", c.configBundle)
	appendFlag("--journal", c.journalPath)
	args = append(args, "run")
	beforeDelimiter := true
	for i, arg := range runArgs {
		if beforeDelimiter && arg == "--background" {
			continue
		}
		if arg == "--" && addExecutionID {
			args = append(args, "--execution-id", executionID.String())
			addExecutionID = false
		}
		if arg == "--" {
			beforeDelimiter = false
		}
		args = append(args, runArgs[i])
	}
	return args
}

func readBackgroundExecution(ctx context.Context, journalPath string, executionID ids.ExecutionID) (execution model.Execution, found bool, problem *output.Error) {
	if _, err := os.Stat(journalPath); errors.Is(err, os.ErrNotExist) {
		return execution, false, nil
	} else if err != nil {
		return execution, false, output.Wrap(output.CodeAuthorizationDenied, "inspect background execution journal", false, err).WithDetail("execution_id", executionID.String())
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true, LockTimeout: 100 * time.Millisecond})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrBusy) {
		return execution, false, nil
	}
	if err != nil {
		return execution, false, mapStoreError("open background execution journal", err).WithDetail("execution_id", executionID.String())
	}
	defer journal.Close()
	execution, err = journal.GetExecution(ctx, executionID)
	if errors.Is(err, store.ErrNotFound) {
		return execution, false, nil
	}
	if err != nil {
		return execution, false, mapStoreError("read background execution", err).WithDetail("execution_id", executionID.String())
	}
	return execution, true, nil
}
