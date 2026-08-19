package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const backgroundStartupTimeout = 30 * time.Second

const backgroundJournalReadRetry = 2 * time.Second

const backgroundReadyTokenEnv = "AGENTCTL_BACKGROUND_READY_TOKEN"

type backgroundReady struct {
	Token       string `json:"agentctl_background_ready"`
	ExecutionID string `json:"execution_id"`
}

func (a *app) runNativeBackground(ctx context.Context, renderer output.Renderer, c common, args []string, opts runOptions, prompt *promptPayload) *output.Error {
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
	workerArgs := backgroundCommandArgs(c, args, executionID, opts.executionID.IsZero(), prompt != nil)
	cmd := exec.Command(executable, workerArgs...)
	if prompt != nil {
		// The parent materializes bounded prompt bytes before detaching. An
		// inherited one-shot pipe keeps raw prompts out of argv, repositories, and
		// the durable journal while allowing the worker to consume stdin safely.
		cmd.Stdin = bytes.NewReader(prompt.Bytes)
	}
	cmd.Stderr = nil
	readyToken, err := newBackgroundReadyToken()
	if err != nil {
		return output.Wrap(output.CodeInternal, "allocate background readiness token", false, err)
	}
	cmd.Env = append(environmentWithout(os.Environ(), backgroundReadyTokenEnv), backgroundReadyTokenEnv+"="+readyToken)
	workerOutput, err := cmd.StdoutPipe()
	if err != nil {
		return output.Wrap(output.CodeInternal, "open background worker readiness pipe", false, err)
	}
	defer workerOutput.Close()
	prepareDetachedCommand(cmd)
	if err := cmd.Start(); err != nil {
		return output.Wrap(output.CodeDependencyUnavailable, "start background agentctl worker", true, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	line := make(chan []byte, 1)
	go func() {
		value, _ := bufio.NewReader(io.LimitReader(workerOutput, 64*1024)).ReadBytes('\n')
		line <- value
	}()
	deadline := time.NewTimer(backgroundStartupTimeout)
	defer deadline.Stop()
	for {
		select {
		case raw := <-line:
			var ready backgroundReady
			if err := json.Unmarshal(raw, &ready); err == nil && ready.Token == readyToken && ready.ExecutionID == executionID.String() {
				execution, found, problem := readBackgroundExecution(context.Background(), journalPath, executionID)
				if problem != nil {
					return problem
				}
				if !found {
					return output.NewError(output.CodeInternal, "background worker acknowledged an execution missing from the journal", false).WithDetail("execution_id", executionID.String())
				}
				return writeExecution(renderer, execution, "background")
			}
			return backgroundWorkerError(raw, executionID, nil)
		case waitErr := <-done:
			select {
			case raw := <-line:
				return backgroundWorkerError(raw, executionID, waitErr)
			default:
			}
			return backgroundWorkerError(nil, executionID, waitErr)
		case <-ctx.Done():
			return output.Wrap(output.CodeExecutionCancelled, "background launch observation cancelled; the detached worker may continue", false, ctx.Err()).WithDetail("execution_id", executionID.String()).WithDetail("worker_continues", true)
		case <-deadline.C:
			return output.NewError(output.CodeTimeout, "background worker did not acknowledge durable startup before the deadline and may continue", true).WithDetail("execution_id", executionID.String()).WithDetail("timeout", backgroundStartupTimeout.String()).WithDetail("worker_continues", true)
		}
	}
}

func emitBackgroundReady(writer io.Writer, executionID ids.ExecutionID) bool {
	token := os.Getenv(backgroundReadyTokenEnv)
	if token == "" {
		return false
	}
	_ = os.Unsetenv(backgroundReadyTokenEnv)
	_ = json.NewEncoder(writer).Encode(backgroundReady{Token: token, ExecutionID: executionID.String()})
	return true
}

func newBackgroundReadyToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func environmentWithout(values []string, name string) []string {
	prefix := name + "="
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) >= len(prefix) && value[:len(prefix)] == prefix {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func backgroundWorkerError(raw []byte, executionID ids.ExecutionID, waitErr error) *output.Error {
	var document output.ErrorDocument
	if json.Unmarshal(raw, &document) == nil && document.Error != nil {
		document.Error.Details["execution_id"] = executionID.String()
		return document.Error
	}
	detail := "worker exited before acknowledging durable startup"
	if waitErr != nil {
		detail = waitErr.Error()
	}
	return output.NewError(output.CodeExecutionFailed, "background agentctl worker failed during startup", false).WithDetail("execution_id", executionID.String()).WithDetail("worker_error", detail)
}

func backgroundCommandArgs(c common, runArgs []string, executionID ids.ExecutionID, addExecutionID bool, materializedPrompt bool) []string {
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
	for i := 0; i < len(runArgs); i++ {
		arg := runArgs[i]
		if beforeDelimiter && arg == "--background" {
			continue
		}
		if beforeDelimiter && materializedPrompt && arg == "--prompt-file" {
			if i+1 < len(runArgs) {
				i++
			}
			args = append(args, "--prompt-stdin")
			continue
		}
		if arg == "--" && addExecutionID {
			args = append(args, "--execution-id", executionID.String())
			addExecutionID = false
		}
		if arg == "--" {
			beforeDelimiter = false
		}
		args = append(args, arg)
	}
	return args
}

func readBackgroundExecution(ctx context.Context, journalPath string, executionID ids.ExecutionID) (execution model.Execution, found bool, problem *output.Error) {
	if _, err := os.Stat(journalPath); errors.Is(err, os.ErrNotExist) {
		return execution, false, nil
	} else if err != nil {
		return execution, false, output.Wrap(output.CodeAuthorizationDenied, "inspect background execution journal", false, err).WithDetail("execution_id", executionID.String())
	}
	var journal *store.Journal
	var err error
	deadline := time.Now().Add(backgroundJournalReadRetry)
	for {
		journal, err = store.Open(journalPath, store.Options{ReadOnly: true, LockTimeout: 100 * time.Millisecond})
		if !errors.Is(err, store.ErrBusy) || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return execution, false, output.Wrap(output.CodeExecutionCancelled, "inspect background execution journal", false, ctx.Err()).WithDetail("execution_id", executionID.String())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if errors.Is(err, store.ErrNotFound) {
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
