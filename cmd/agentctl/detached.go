package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

// This private, same-binary protocol exists only in an anonymous pipe. In
// particular, Prompt must never be written to a journal, spool, or argv.
type detachedLaunch struct {
	ResumeSession                                  string `json:",omitempty"`
	Version                                        int
	Profile, ConfigPath, ConfigBundle, JournalPath string
	Adapter, CWD                                   string
	Env, Argv, Labels                              []string
	Timeout                                        time.Duration
	Binding                                        *model.DelegationBinding
	Mutation                                       contracts.MutationKey
	Prompt                                         promptPayload
	ReservedExecution                              ids.ExecutionID
	JournalBusyBudget                              time.Duration
	DeadlineAt                                     *time.Time
}

type detachedReply struct {
	OK          bool                `json:"ok"`
	Result      json.RawMessage     `json:"result"`
	Error       *output.Error       `json:"error"`
	NextActions []output.NextAction `json:"next_actions"`
}

func (a *app) startDetachedNative(ctx context.Context, renderer output.Renderer, c common, run runOptions) *output.Error {
	// Admission belongs to the collecting parent. Once a worker can exist,
	// every outcome has a durable execution identity, including a lost pipe.
	run.detachNative = true
	return a.runNativeOptions(ctx, renderer, c, nil, run)
}

func (a *app) startReservedDetachedNative(ctx context.Context, renderer output.Renderer, c common, run runOptions, execution model.Execution) (problem *output.Error) {
	workerStarted := false
	defer func() {
		if problem == nil {
			return
		}
		problem.WithDetail("execution_id", execution.ID.String())
		if !workerStarted {
			a.settleDetachedAdmission(c, execution.ID, problem)
		}
	}()
	journalPath, err := a.journalPath(c)
	if err != nil {
		return output.Wrap(output.CodeUsage, "resolve detached journal", false, err)
	}
	absolute := func(path string) (string, error) {
		if path == "" {
			return "", nil
		}
		return filepath.Abs(path)
	}
	journalPath, err = filepath.Abs(journalPath)
	if err != nil {
		return output.Wrap(output.CodeUsage, "resolve detached journal", false, err)
	}
	configPath, err := absolute(c.configPath)
	if err != nil {
		return output.Wrap(output.CodeUsage, "resolve detached config", false, err)
	}
	configBundle, err := absolute(c.configBundle)
	if err != nil {
		return output.Wrap(output.CodeUsage, "resolve detached config bundle", false, err)
	}
	payload := detachedLaunch{ResumeSession: run.resumeSession, Version: 2, Profile: c.profile, ConfigPath: configPath, ConfigBundle: configBundle, JournalPath: journalPath,
		Adapter: run.adapter, CWD: run.cwd, Env: run.env, Argv: run.argv, Labels: run.labels, Timeout: run.timeout,
		Binding: run.delegation, Mutation: *run.mutationOverride, Prompt: *run.preparedPrompt, ReservedExecution: execution.ID, JournalBusyBudget: c.journalBusyBudget, DeadlineAt: execution.DeadlineAt}
	data, err := json.Marshal(payload)
	if err != nil {
		return output.Wrap(output.CodeInternal, "encode detached launch", false, err)
	}
	executable, err := os.Executable()
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve agentctl executable", false, err)
	}
	// Never CommandContext: cancellation of the collecting parent must not kill
	// the owner. Its native execution deadline is passed separately above.
	command := exec.Command(executable, "_delegate-worker")
	command.Stdin = bytes.NewReader(data)
	command.Env = os.Environ()
	prepareDetachedCommand(command)
	replyPipe, err := command.StdoutPipe()
	if err != nil {
		return output.Wrap(output.CodeInternal, "open detached receipt pipe", false, err)
	}
	startWorker := command.Start
	if a.startDetachedWorker != nil {
		startWorker = func() error { return a.startDetachedWorker(command) }
	}
	if err := startWorker(); err != nil {
		replyPipe.Close()
		return output.Wrap(output.CodeDependencyUnavailable, "start detached owner", false, err)
	}
	workerStarted = true
	// Wait reaps a worker that exits while this caller is still alive. The pipe
	// reader owns receipt collection; Wait must not close it early.
	type response struct {
		reply detachedReply
		err   error
	}
	received := make(chan response, 1)
	go func() {
		var reply detachedReply
		err := json.NewDecoder(io.LimitReader(replyPipe, 1<<20)).Decode(&reply)
		replyPipe.Close()
		received <- response{reply, err}
		_ = command.Wait()
	}()
	// The worker must have at least the same lock budget as its parent.
	timer := time.NewTimer(c.journalBusyBudget + time.Minute)
	defer timer.Stop()
	known := func() *output.Error {
		// A missing worker receipt says nothing about native success. Return
		// the committed admission (starting) so await/status can collect it.
		return a.collectDelegation(context.Background(), renderer, c, execution, delegateOptions{detachedReceipt: true}, false)
	}
	select {
	case <-ctx.Done():
		return known()
	case <-timer.C:
		return known()
	case response := <-received:
		if response.err != nil {
			return known()
		}
		if !response.reply.OK {
			if response.reply.Error == nil {
				return known()
			}
			// Even a legacy worker that refuses this protocol leaves its
			// parent's exact admission known and terminal when no native
			// process was accepted.
			a.settleDetachedAdmission(c, execution.ID, response.reply.Error)
			return response.reply.Error
		}
		if len(response.reply.Result) == 0 {
			return known()
		}
		var metadata struct {
			ID     string      `json:"id"`
			State  model.State `json:"state"`
			Reused bool        `json:"reused"`
		}
		if err := json.Unmarshal(response.reply.Result, &metadata); err != nil || metadata.ID != execution.ID.String() {
			return known()
		}
		if err := renderer.Success(output.Success{Result: response.reply.Result, NextActions: response.reply.NextActions,
			Lines: []output.Line{{Lead: metadata.ID, Fields: []output.Field{{Name: "state", Value: metadata.State}, {Name: "reused", Value: metadata.Reused}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write detached receipt", false, err)
		}
		return nil
	}
}

func (a *app) detachedNativeWorker(ctx context.Context) int {
	ignoreDetachedPipeSignal()
	renderer := output.Renderer{Mode: output.JSON, Writer: a.stdout}
	var payload detachedLaunch
	// JSON base64 encoding expands an 8 MiB prompt to less than 12 MiB.
	decoder := json.NewDecoder(io.LimitReader(a.stdin, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || (payload.Version != 1 && payload.Version != 2) || payload.Binding == nil ||
		!payload.Mutation.Enabled() || len(payload.Argv) == 0 || len(payload.Prompt.Bytes) > maxPromptBytes ||
		payload.Prompt.Digest != sha256Digest(payload.Prompt.Bytes) || (payload.Version == 2 && payload.ReservedExecution.IsZero()) || payload.JournalBusyBudget < 0 || payload.JournalBusyBudget > maxJournalBusyBudget {
		return a.fail(renderer, delegateError(output.CodeUsage, "detached_launch_invalid", "invalid internal detached launch"))
	}
	c := common{mode: output.JSON, profile: payload.Profile, configPath: payload.ConfigPath, configBundle: payload.ConfigBundle, journalPath: payload.JournalPath}
	// Accept a v1 parent that crossed an atomic binary swap. Its admission
	// contract is unchanged; v2 parents always pass a reservation and budget.
	c.journalBusyBudget = payload.JournalBusyBudget
	if payload.Version == 1 {
		if budget, err := resolveJournalBusyBudget("delegate", "", false, a.getenv); err == nil {
			c.journalBusyBudget = budget
		} else {
			c.journalBusyBudget = defaultJournalBusyBudget
		}
	}
	if payload.Version == 2 && payload.DeadlineAt != nil {
		// Admission already fixed the execution's deadline. Worker startup
		// and a second version probe must not grant it another full timeout.
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, *payload.DeadlineAt)
		defer cancel()
	}
	sent := false
	sendReceipt := func(execution model.Execution, reused bool) {
		sent = true
		// Delivery failure must never kill accepted work. A retry recovers the
		// immutable mutation binding even when the parent lost this response.
		_ = a.collectDelegation(context.Background(), renderer, c, execution, delegateOptions{detachedReceipt: true}, reused)
		if closer, ok := a.stdout.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	run := runOptions{resumeSession: payload.ResumeSession, env: payload.Env, adapter: payload.Adapter, cwd: payload.CWD, argv: payload.Argv, labels: payload.Labels,
		timeout: payload.Timeout, delegation: payload.Binding, mutationOverride: &payload.Mutation, preparedPrompt: &payload.Prompt, started: sendReceipt, reservedExecution: payload.ReservedExecution}
	problem := a.runNativeOptions(ctx, output.Renderer{Mode: output.JSON, Writer: io.Discard}, c, nil, run)
	if problem != nil {
		if !sent {
			if !payload.ReservedExecution.IsZero() {
				a.settleDetachedAdmission(c, payload.ReservedExecution, problem)
			}
			return a.fail(renderer, problem)
		}
		return problem.ExitCode
	}
	return 0
}

func (a *app) settleDetachedAdmission(c common, id ids.ExecutionID, problem *output.Error) {
	problem.WithDetail("execution_id", id.String())
	journal, current, openProblem := a.openExecutionWriteOwned(c, id)
	if openProblem != nil {
		problem.WithDetail("terminal_recorded", false)
		return
	}
	defer journal.Close()
	if current.State.Terminal() {
		problem.WithDetail("terminal_recorded", true)
		return
	}
	// Never settle another worker's accepted native process from this path.
	if current.State != model.StateStarting || len(current.SourceBindings) != 0 || problem.Code == output.CodeConflict {
		return
	}
	state := adapter.StateFailed
	if problem.Code == output.CodeTimeout || problem.Code == output.CodeExecutionCancelled {
		state = adapter.StateCancelled
	}
	result := adapter.Result{Success: false, State: state, Error: problem.Message, Data: map[string]any{"diagnostic_code": "detached_worker_start_failed"}}
	if code, _ := problem.Details["diagnostic_code"].(string); code == "native_version_probe_failed" || code == "native_version_probe_timeout" || code == "macos_login_keychain_locked" || code == "journal_busy" {
		result.Data["diagnostic_code"] = code
	}
	if value, ok := problem.Details["native_exit_code"]; ok {
		switch code := value.(type) {
		case int:
			result.ExitCode = &code
		case float64:
			converted := int(code)
			if float64(converted) == code {
				result.ExitCode = &converted
			}
		}
	}
	_, err := a.finalizeResult(context.Background(), c, journal, current, result, a.now().UTC(), false)
	problem.WithDetail("terminal_recorded", err == nil)
}

func writeDetachedPlan(renderer output.Renderer, binding model.DelegationBinding, preflight json.RawMessage) *output.Error {
	plan := binding.NativePlan
	native := delegatePlanNative(plan.Env, plan.Argv, plan.PromptDelivery, plan.Permissions, preflight)
	var captured bytes.Buffer
	if problem := writeDelegatePlan(output.Renderer{Mode: output.JSON, Writer: &captured}, binding, native, ""); problem != nil {
		return problem
	}
	var document struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(captured.Bytes(), &document); err != nil {
		return output.Wrap(output.CodeInternal, "decode detached plan", false, err)
	}
	document.Result["lifecycle"] = map[string]any{"owner": "detached_process", "parent_independent": true, "restart_durable": false,
		"reboot_recovery": false, "collection": "await <id> --content; stops on terminal state or attention", "authority": "native"}
	if err := renderer.Success(output.Success{Result: document.Result, Lines: []output.Line{{Lead: "delegate.plan", Fields: []output.Field{{Name: "owner", Value: "detached_process"}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write detached plan", false, err)
	}
	return nil
}
