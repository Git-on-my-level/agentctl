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

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

// This private, same-binary protocol exists only in an anonymous pipe. In
// particular, Prompt must never be written to a journal, spool, or argv.
type detachedLaunch struct {
	Version                                        int
	Profile, ConfigPath, ConfigBundle, JournalPath string
	Adapter, CWD                                   string
	Env, Argv, Labels                              []string
	Timeout                                        time.Duration
	Binding                                        *model.DelegationBinding
	Mutation                                       contracts.MutationKey
	Prompt                                         promptPayload
}

type detachedReply struct {
	OK          bool                `json:"ok"`
	Result      json.RawMessage     `json:"result"`
	Error       *output.Error       `json:"error"`
	NextActions []output.NextAction `json:"next_actions"`
}

func (a *app) startDetachedNative(ctx context.Context, renderer output.Renderer, c common, run runOptions) *output.Error {
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
	payload := detachedLaunch{Version: 1, Profile: c.profile, ConfigPath: configPath, ConfigBundle: configBundle, JournalPath: journalPath,
		Adapter: run.adapter, CWD: run.cwd, Env: run.env, Argv: run.argv, Labels: run.labels, Timeout: run.timeout,
		Binding: run.delegation, Mutation: *run.mutationOverride, Prompt: *run.preparedPrompt}
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
	if err := command.Start(); err != nil {
		replyPipe.Close()
		return output.Wrap(output.CodeDependencyUnavailable, "start detached owner", false, err)
	}
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
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	uncertain := func() *output.Error {
		return output.NewError(output.CodeExecutionUnknown, "detached launch receipt unavailable; replay the same request key and inputs to recover work; no relaunch was authorized", true).
			WithDetail("diagnostic_code", "detached_receipt_unknown")
	}
	select {
	case <-ctx.Done():
		return uncertain()
	case <-timer.C:
		return uncertain()
	case response := <-received:
		if response.err != nil {
			return uncertain()
		}
		if !response.reply.OK {
			if response.reply.Error == nil {
				return uncertain()
			}
			return response.reply.Error
		}
		if len(response.reply.Result) == 0 {
			return uncertain()
		}
		var metadata struct {
			ID     string      `json:"id"`
			State  model.State `json:"state"`
			Reused bool        `json:"reused"`
		}
		if err := json.Unmarshal(response.reply.Result, &metadata); err != nil || metadata.ID == "" {
			return uncertain()
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
	if err := decoder.Decode(&payload); err != nil || payload.Version != 1 || payload.Binding == nil ||
		!payload.Mutation.Enabled() || len(payload.Argv) == 0 || len(payload.Prompt.Bytes) > maxPromptBytes ||
		payload.Prompt.Digest != sha256Digest(payload.Prompt.Bytes) {
		return a.fail(renderer, delegateError(output.CodeUsage, "detached_launch_invalid", "invalid internal detached launch"))
	}
	c := common{mode: output.JSON, profile: payload.Profile, configPath: payload.ConfigPath, configBundle: payload.ConfigBundle, journalPath: payload.JournalPath}
	// The worker inherits the launcher's environment, so an operator budget
	// override applies here too; otherwise it keeps the delegate default.
	if budget, err := resolveJournalBusyBudget("delegate", "", false, a.getenv); err == nil {
		c.journalBusyBudget = budget
	} else {
		c.journalBusyBudget = defaultJournalBusyBudget
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
	run := runOptions{env: payload.Env, adapter: payload.Adapter, cwd: payload.CWD, argv: payload.Argv, labels: payload.Labels,
		timeout: payload.Timeout, delegation: payload.Binding, mutationOverride: &payload.Mutation, preparedPrompt: &payload.Prompt, started: sendReceipt}
	problem := a.runNativeOptions(ctx, output.Renderer{Mode: output.JSON, Writer: io.Discard}, c, nil, run)
	if problem != nil {
		if !sent {
			return a.fail(renderer, problem)
		}
		return problem.ExitCode
	}
	return 0
}

func writeDetachedPlan(renderer output.Renderer, binding model.DelegationBinding, preflight json.RawMessage) *output.Error {
	plan := binding.NativePlan
	native := map[string]any{"env": plan.Env, "argv": plan.Argv, "prompt_delivery": plan.PromptDelivery, "permissions": plan.Permissions, "preflight": preflight}
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
