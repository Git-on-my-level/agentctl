package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/delegation"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// SSH authenticates and routes using the operator's configuration. One child
// per operation, no resident transport and no local native-adapter probes.
func sshDestination(profile config.Profile, host string) string {
	if profile.Route != nil {
		if value, ok := profile.Route.SSHHosts[host]; ok {
			return value
		}
	}
	return host
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func remoteCommand(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// Drain bounded stdout and retain the diagnostic tail; never log stdin.
type sshCapture struct {
	bytes.Buffer
	limit          int
	tail, overflow bool
}

func (b *sshCapture) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > b.limit {
		b.overflow = true
	}
	if b.tail {
		if n >= b.limit {
			b.Reset()
			_, _ = b.Buffer.Write(p[n-b.limit:])
		} else {
			if b.Len()+n > b.limit {
				b.Next(b.Len() + n - b.limit)
			}
			_, _ = b.Buffer.Write(p)
		}
	} else if remaining := b.limit - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

// Preserve diagnostics through receipt validation as well as process failure.
type sshResponse struct {
	body           []byte
	diagnosticTail string
}

func (r sshResponse) problem(problem *output.Error) *output.Error {
	if problem != nil && r.diagnosticTail != "" {
		problem.WithDetail("remote_diagnostic_tail", r.diagnosticTail)
	}
	return problem
}
func (r sshResponse) result() (json.RawMessage, *output.Error) {
	raw, problem := remoteResult(r.body)
	return raw, r.problem(problem)
}
func (r sshResponse) receipt() (remoteReceipt, *output.Error) {
	receipt, problem := parseRemoteReceipt(r.body)
	return receipt, r.problem(problem)
}

func sshStep(ctx context.Context, destination, command string, stdin []byte) (sshResponse, *output.Error) {
	binary := os.Getenv("AGENTCTL_SSH")
	if binary == "" {
		binary = "ssh"
	}
	cmd := exec.CommandContext(ctx, binary, "--", destination, command)
	cmd.Stdin = bytes.NewReader(stdin)
	stdout := sshCapture{limit: 8 << 20}
	stderr := sshCapture{limit: 4096, tail: true}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	tail := strings.ToValidUTF8(stderr.String(), "�")
	annotate := func(problem *output.Error) *output.Error {
		if tail != "" {
			problem.WithDetail("remote_diagnostic_tail", tail)
		}
		return problem
	}
	if ctx.Err() != nil {
		code := output.CodeExecutionCancelled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = output.CodeTimeout
		}
		return sshResponse{}, annotate(output.NewError(code, "SSH operation interrupted; retry the same request key to recover admission", true))
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if exit.ExitCode() != 255 && (exit.ExitCode() == 127 || strings.Contains(strings.ToLower(tail), "command not found")) {
				return sshResponse{}, annotate(delegateError(output.CodeDependencyUnavailable, "delegate_remote_agentctl_missing", "remote agentctl is unavailable; bootstrap agentctl on the destination").WithActions(output.NextAction{Label: "Inspect bootstrap instructions", Argv: []string{"agentctl", "help", "bootstrap"}, SideEffectClass: output.ReadOnly}))
			}
			if exit.ExitCode() != 255 {
				var document output.ErrorDocument
				if json.Unmarshal(stdout.Bytes(), &document) == nil && !document.OK && document.SchemaVersion == 1 && document.Error != nil && document.Error.ExitCode == exit.ExitCode() && output.ExitCodeFor(document.Error.Code) == exit.ExitCode() {
					if document.Error.Details == nil {
						document.Error.Details = map[string]any{}
					}
					return sshResponse{}, annotate(document.Error)
				}
				return sshResponse{}, annotate(delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote command returned an invalid error receipt").WithDetail("remote_exit_code", exit.ExitCode()))
			}
		}
		return sshResponse{}, annotate(output.Wrap(output.CodeRemoteFailure, "SSH failed; retry the same request key", true, err))
	}
	if stdout.overflow {
		return sshResponse{}, annotate(delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote output exceeds the bounded receipt limit"))
	}
	if !strings.HasPrefix(command, "umask ") {
		if _, problem := remoteResult(stdout.Bytes()); problem != nil {
			return sshResponse{}, annotate(problem)
		}
	}
	return sshResponse{body: stdout.Bytes(), diagnosticTail: tail}, nil
}

func remoteResult(body []byte) (json.RawMessage, *output.Error) {
	var document struct {
		OK            bool            `json:"ok"`
		SchemaVersion int             `json:"schema_version"`
		Result        json.RawMessage `json:"result"`
	}
	if json.Unmarshal(body, &document) != nil || !document.OK || document.SchemaVersion != 1 || len(document.Result) == 0 || document.Result[0] != '{' {
		return nil, delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote agentctl returned an invalid receipt")
	}
	return document.Result, nil
}

type remoteReceipt struct {
	ID           ids.ExecutionID             `json:"id"`
	OriginHostID ids.HostID                  `json:"origin_host_id"`
	State        model.State                 `json:"state"`
	Liveness     model.Liveness              `json:"liveness"`
	Resolved     model.DelegationTarget      `json:"resolved"`
	Capacity     *model.DelegationCapacity   `json:"capacity"`
	Continuation *model.FollowUpContinuation `json:"continuation"`
	Provenance   struct {
		ConfigurationSHA256 string   `json:"configuration_sha256"`
		Defaulted           []string `json:"defaulted"`
	} `json:"provenance"`
}

func parseRemoteReceipt(body []byte) (remoteReceipt, *output.Error) {
	var receipt remoteReceipt
	raw, problem := remoteResult(body)
	if problem != nil {
		return receipt, problem
	}
	err := json.Unmarshal(raw, &receipt)
	_, idErr := ids.ParseExecutionID(receipt.ID.String())
	_, hostErr := ids.ParseHostID(receipt.OriginHostID.String())
	validState := false
	switch receipt.State {
	case model.StateCreated, model.StateStarting, model.StateRunning, model.StateWaiting, model.StateAttention, model.StateCompleted, model.StateFailed, model.StateCancelled, model.StateOrphaned:
		validState = true
	}
	validLiveness := receipt.Liveness == ""
	switch receipt.Liveness {
	case model.LivenessUnknown, model.LivenessAlive, model.LivenessBlocked, model.LivenessExited, model.LivenessUnreachable:
		validLiveness = true
	}
	if err != nil || idErr != nil || hostErr != nil || !validState || !validLiveness {
		return receipt, delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote receipt lacks a valid execution identity or state")
	}
	return receipt, nil
}

func remoteLaunchFlags(opts delegateOptions) []string {
	args := []string{"--prompt-stdin", "--detach"}
	if opts.plan {
		args = append(args, "--plan")
	}
	if opts.timeout > 0 {
		args = append(args, "--timeout", opts.timeout.String())
	}
	for _, label := range opts.labels {
		args = append(args, "--label", label)
	}
	return args
}

func (a *app) delegateRemote(ctx context.Context, renderer output.Renderer, c common, profile config.Profile, host string, request delegation.Request, requestBody, prompt []byte, opts delegateOptions, mutation contracts.MutationKey, replay ...model.Execution) (problem *output.Error) {
	defer func() {
		if problem != nil && len(replay) != 0 {
			problem = remoteErrorLocal(problem, replay[0])
		}
	}()
	destination := sshDestination(profile, host)
	if len(replay) != 0 {
		destination = replay[0].Delegation.RemotePlan.SSHDestination
	}
	name := "agentctl-delegate-" + strings.TrimPrefix(sha256Digest(requestBody), "sha256:")[:16] + ".json"
	// TMPDIR is expanded only by the remote shell, with a portable fallback.
	staged := `"${TMPDIR:-/tmp}/` + name + `"`
	if _, problem := sshStep(ctx, destination, "umask 077 && cat > "+staged, requestBody); problem != nil {
		return problem
	}
	args := append(remoteLaunchFlags(opts), "--authority", "native")
	command := "cd " + shellQuote(opts.cwd) + " && " + remoteCommand("agentctl", "delegate", "--output", "json", "--cwd", opts.cwd) + " --request-file " + staged + " " + remoteCommand(args...)
	// Let agentctl classify a missing cwd rather than allowing shell cd to hide it.
	command = "if [ -d " + shellQuote(opts.cwd) + " ]; then " + command + "; else " + remoteCommand("agentctl", "delegate", "--output", "json", "--cwd", opts.cwd) + " --request-file " + staged + " " + remoteCommand(args...) + "; fi"
	body, problem := sshStep(ctx, destination, command, prompt)
	if problem != nil {
		return problem
	}
	if opts.plan {
		return writeRemotePlan(renderer, body, host, destination, true)
	}
	if len(replay) != 0 {
		receipt, problem := body.receipt()
		if problem != nil {
			return problem
		}
		remote := replay[0].Delegation.RemotePlan
		if receipt.ID != remote.ExecutionID || receipt.OriginHostID != remote.OriginHostID {
			return body.problem(delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote replay changed the admitted identity"))
		}
		return a.collectDelegation(ctx, renderer, c, replay[0], opts, true)
	}
	return a.recordRemote(ctx, renderer, c, host, destination, request.Selector, request.RequestKey, body, opts, mutation, nil)
}

func writeRemotePlan(renderer output.Renderer, body sshResponse, host, destination string, staged bool, source ...ids.ExecutionID) *output.Error {
	raw, problem := body.result()
	if problem != nil {
		return problem
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil || fields["plan"] != true {
		return body.problem(delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote plan receipt is invalid"))
	}
	transport := map[string]any{"host": host, "ssh_destination": destination, "request_staged": staged, "journal_written": false}
	// A plan admits no local execution. Any remote replay identity belongs to
	// transport provenance, never to the caller's local handle namespace.
	for _, field := range []string{"id", "origin_host_id", "continues", "reused"} {
		if value, ok := fields[field]; ok {
			transport["remote_"+field] = value
		}
	}
	delete(fields, "id")
	delete(fields, "origin_host_id")
	fields["reused"] = false
	if len(source) != 0 {
		fields["continues"] = source[0]
	} else {
		delete(fields, "continues")
	}
	fields["transport"] = transport
	if staged {
		fields["side_effect_class"] = output.ExternalSideEffect
	}
	if err := renderer.Success(output.Success{Result: fields, Lines: []output.Line{{Lead: "delegate.plan", Fields: []output.Field{{Name: "host", Value: host}, {Name: "transport", Value: "ssh"}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write remote plan", false, err)
	}
	return nil
}

func (a *app) recordRemote(ctx context.Context, renderer output.Renderer, c common, host, destination string, selector delegation.Selector, key string, body sshResponse, opts delegateOptions, mutation contracts.MutationKey, source *model.Execution) *output.Error {
	receipt, problem := body.receipt()
	if problem != nil {
		return problem
	}
	journaled := selector
	if selector.Pool != "" {
		// Match native pool admission's rollback-safe projection. Transport
		// still forwards the original selector and capacity stays remote-owned.
		journaled = pooledJournalSelector(selector, delegation.Entry{Harness: receipt.Resolved.Harness, Model: receipt.Resolved.Model})
	}
	requested, _ := json.Marshal(journalSelector(journaled))
	resolved := receipt.Resolved
	resolved.Host = host
	binding := &model.DelegationBinding{RemotePlan: &model.DelegationRemotePlan{Host: host, SSHDestination: destination, ExecutionID: receipt.ID, OriginHostID: receipt.OriginHostID}, RequestSHA256: mutation.InputDigest, ConfigurationSHA256: receipt.Provenance.ConfigurationSHA256, Requested: requested, RequestKey: key, Resolved: resolved, Defaulted: append([]string{}, receipt.Provenance.Defaulted...), Capacity: receipt.Capacity, Continuation: receipt.Continuation}
	if err := binding.Validate(); err != nil {
		return body.problem(delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote resolution binding is invalid"))
	}
	if resolved.Authority != model.AuthorityNative {
		return body.problem(delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote receipt must have native authority"))
	}
	now := a.now().UTC()
	state := receipt.State
	execution := model.Execution{Authority: model.AuthorityNative, Adapter: resolved.Harness, Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: state, Liveness: model.LivenessUnknown, Labels: opts.labels, CWD: &opts.cwd, Delegation: binding, Capabilities: model.CapabilitySnapshot{AdapterVersion: "remote-receipt-v1", NegotiatedAt: now}, Observation: model.Observation{Source: model.ObservationStatusAPI, Integrity: model.IntegrityVerified, ObservedAt: now}}
	if state.Terminal() {
		execution.TerminalAt = &now
	}
	if opts.timeout > 0 {
		deadline := now.Add(opts.timeout)
		execution.DeadlineAt = &deadline
	}
	if source != nil {
		execution.Supersedes = []ids.ExecutionID{source.ID}
		execution.CWD = source.CWD
	}
	journal, problem := a.openWriteContext(ctx, c)
	if problem != nil {
		return problem
	}
	execution, reused, err := journal.CreateExecution(ctx, execution, mutation)
	journal.Close()
	if err != nil {
		return mapStoreError("record remote admission", err)
	}
	if !execution.RemoteForwarded() || execution.Delegation.RemotePlan.ExecutionID != receipt.ID || execution.Delegation.RemotePlan.OriginHostID != receipt.OriginHostID {
		return delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote admission disagrees with the frozen local binding").WithDetail("execution_id", execution.ID.String())
	}
	opts.detachedReceipt = true
	return a.collectDelegation(ctx, renderer, c, execution, opts, reused)
}

func remoteErrorLocal(problem *output.Error, execution model.Execution) *output.Error {
	if problem == nil {
		return nil
	}
	problem.WithDetail("execution_id", execution.ID.String()).WithDetail("remote", execution.Delegation.RemotePlan)
	remote := execution.Delegation.RemotePlan
	for i := range problem.NextActions {
		for j, arg := range problem.NextActions[i].Argv {
			if arg == remote.ExecutionID.String() {
				problem.NextActions[i].Argv[j] = execution.ID.String()
			}
		}
	}
	return problem
}

func remoteObservation(ctx context.Context, execution model.Execution, command string, flags []string) (remoteReceipt, *output.Error) {
	plan := execution.Delegation.RemotePlan
	args := append([]string{"agentctl", command, plan.ExecutionID.String(), "--output", "json"}, flags...)
	body, problem := sshStep(ctx, plan.SSHDestination, remoteCommand(args...), nil)
	if problem != nil {
		return remoteReceipt{}, remoteErrorLocal(problem, execution)
	}
	receipt, problem := body.receipt()
	if problem == nil && (receipt.ID != plan.ExecutionID || receipt.OriginHostID != plan.OriginHostID) {
		problem = delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote observation identity disagrees with admitted execution")
	}
	return receipt, remoteErrorLocal(body.problem(problem), execution)
}

func (a *app) statusRemote(ctx context.Context, renderer output.Renderer, execution model.Execution) *output.Error {
	receipt, problem := remoteObservation(ctx, execution, "status", nil)
	if problem != nil {
		return problem
	}
	execution.State = receipt.State
	if receipt.Liveness != "" {
		execution.Liveness = receipt.Liveness
	}
	if execution.State.Terminal() {
		now := a.now().UTC()
		execution.TerminalAt = &now
	}
	execution.Observation = model.Observation{Source: model.ObservationStatusAPI, Integrity: model.IntegrityVerified, ObservedAt: a.now().UTC()}
	return writeExecution(renderer, execution, "status")
}

// Remote await --content is raw text even in JSON mode. Collect metadata first,
// then the structured result, retaining the native content source and bounds.
func (a *app) observeRemoteTerminal(ctx context.Context, c common, execution model.Execution, flags []string) (model.Execution, *output.Error) {
	receipt, awaitProblem := remoteObservation(ctx, execution, "await", flags)
	if awaitProblem != nil {
		switch awaitProblem.Code {
		case output.CodeExecutionFailed, output.CodeExecutionCancelled, output.CodeExecutionUnknown:
			var problem *output.Error
			receipt, problem = remoteObservation(ctx, execution, "status", nil)
			if problem != nil {
				return execution, problem
			}
		default:
			return execution, awaitProblem
		}
	}
	if !receipt.State.Terminal() {
		return execution, remoteErrorLocal(delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote await did not return terminal evidence"), execution)
	}
	plan := execution.Delegation.RemotePlan
	body, problem := sshStep(ctx, plan.SSHDestination, remoteCommand("agentctl", "result", plan.ExecutionID.String(), "--output", "json", "--allow-empty"), nil)
	if problem != nil {
		return execution, remoteErrorLocal(problem, execution)
	}
	raw, problem := body.result()
	if problem != nil {
		return execution, remoteErrorLocal(problem, execution)
	}
	var result struct {
		Outcome model.Outcome `json:"outcome"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Outcome.ExecutionID != plan.ExecutionID || result.Outcome.State != receipt.State || result.Outcome.Validate() != nil || result.Outcome.Source != execution.Adapter || result.Outcome.ResultRef != fmt.Sprintf("agentctl://%s/%s", plan.OriginHostID, plan.ExecutionID) {
		return execution, remoteErrorLocal(body.problem(delegateError(output.CodeRemoteFailure, "delegate_remote_receipt_invalid", "remote result disagrees with admitted identity or terminal state")), execution)
	}
	journal, problem := a.openWriteContext(ctx, c)
	if problem != nil {
		return execution, problem
	}
	defer journal.Close()
	// Re-read after SSH: concurrent collectors converge on the one stored outcome.
	latest, err := journal.GetExecution(ctx, execution.ID)
	if err != nil {
		return execution, mapStoreError("read remote execution", err)
	}
	if latest.State.Terminal() {
		if latest.State != receipt.State {
			return execution, delegateError(output.CodeConflict, "delegate_remote_terminal_conflict", "remote terminal state conflicts with the collected outcome")
		}
		if _, err := journal.GetOutcome(ctx, latest.ID); err == nil {
			if awaitProblem != nil {
				awaitProblem.WithDetail("remote_terminal_collected", true)
			}
			return latest, awaitProblem
		} else if !errors.Is(err, store.ErrNotFound) {
			return execution, mapStoreError("read collected remote outcome", err)
		}
	}
	now := a.now().UTC()
	next := latest
	next.State, next.Liveness, next.UpdatedAt, next.TerminalAt = receipt.State, model.LivenessExited, now, &now
	next.Observation = model.Observation{Source: model.ObservationStatusAPI, Integrity: model.IntegrityVerified, ObservedAt: now}
	outcome := result.Outcome
	outcome.ExecutionID = execution.ID
	outcome.ResultRef = fmt.Sprintf("agentctl://%s/%s", execution.OriginHostID, execution.ID)
	outcome.RecordedAt = now
	event, canonical, err := syntheticEvent(next, model.EventTerminal, next.State, map[string]any{"remote_execution_id": plan.ExecutionID.String(), "remote_origin_host_id": plan.OriginHostID.String()}, "remote-await", now)
	if err == nil {
		next, _, _, _, err = journal.CommitTerminalOutcome(ctx, next, latest.Revision, outcome, event, canonical)
	}
	if err != nil {
		return execution, mapStoreError("record remote terminal outcome", err)
	}
	if awaitProblem != nil {
		awaitProblem.WithDetail("remote_terminal_collected", true)
	}
	return next, awaitProblem
}

func (a *app) awaitRemote(ctx context.Context, renderer output.Renderer, c common, execution model.Execution, flags []string, content bool) *output.Error {
	forwarded := []string{}
	for _, flag := range flags {
		if flag != "--content" {
			forwarded = append(forwarded, flag)
		}
	}
	execution, problem := a.observeRemoteTerminal(ctx, c, execution, forwarded)
	if problem != nil {
		if problem.Details["remote_terminal_collected"] == true {
			_ = a.acknowledgeExecution(ctx, c, execution.ID, store.AcknowledgementAwait)
		}
		return problem
	}
	switch execution.State {
	case model.StateCompleted:
		if content {
			return a.writeAwaitContent(ctx, renderer, c, execution.ID)
		}
		if problem := writeExecution(renderer, execution, "await"); problem != nil {
			return problem
		}
	default:
		code := output.CodeExecutionUnknown
		if execution.State == model.StateFailed {
			code = output.CodeExecutionFailed
		}
		if execution.State == model.StateCancelled {
			code = output.CodeExecutionCancelled
		}
		problem := outcomeError(code, "remote execution reached a terminal state", execution)
		_ = a.acknowledgeExecution(ctx, c, execution.ID, store.AcknowledgementAwait)
		return problem
	}
	return a.acknowledgeExecution(ctx, c, execution.ID, store.AcknowledgementAwait)
}

func (a *app) continueRemote(ctx context.Context, renderer output.Renderer, c common, source model.Execution, key string, prompt []byte, opts delegateOptions, mutation contracts.MutationKey) *output.Error {
	plan := source.Delegation.RemotePlan
	if len(opts.labels) == 0 {
		opts.labels = append([]string(nil), source.Labels...)
	}
	args := []string{"agentctl", "continue", plan.ExecutionID.String(), "--output", "json", "--request-key", key, "--mode", opts.continuationMode, "--prompt-stdin"}
	if opts.plan {
		args = append(args, "--plan")
	}
	// continue normally owns a foreground turn. --detach is supported by the
	// remote command so the SSH session is never the execution owner.
	if !opts.plan {
		args = append(args, "--detach")
	}
	if opts.timeout > 0 {
		args = append(args, "--timeout", opts.timeout.String())
	}
	for _, label := range opts.labels {
		args = append(args, "--label", label)
	}
	body, problem := sshStep(ctx, plan.SSHDestination, remoteCommand(args...), prompt)
	if problem != nil {
		return remoteErrorLocal(problem, source)
	}
	if opts.plan {
		return writeRemotePlan(renderer, body, plan.Host, plan.SSHDestination, false, source.ID)
	}
	var selector delegation.Selector
	if json.Unmarshal(source.Delegation.Requested, &selector) != nil {
		return delegateError(output.CodeInternal, "delegate_binding_missing", "remote selector is invalid")
	}
	return a.recordRemote(ctx, renderer, c, plan.Host, plan.SSHDestination, selector, key, body, opts, mutation, &source)
}

var _ io.Writer = (*sshCapture)(nil)
