package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

// Only allowlisted categories and numeric exit codes cross the metadata boundary.
// Upstream stderr can contain prompts, tokens and URLs and is never persisted.
type operationDiagnostic struct {
	Category                string `json:"category"`
	ExitCode                int    `json:"upstream_exit_code"`
	Retryable               bool   `json:"retryable"`
	RemoteCreationUncertain bool   `json:"remote_creation_uncertain"`
}

func (d *operationDiagnostic) Error() string { return "upstream operation: " + d.Category }
func classifyOperation(ctx context.Context, err error, stderr string) *operationDiagnostic {
	d := &operationDiagnostic{Category: "upstream_failure", ExitCode: -1, Retryable: true, RemoteCreationUncertain: true}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		d.ExitCode = exit.ExitCode()
	}
	text := strings.ToLower(stderr)
	switch {
	case ctx.Err() != nil:
		d.Category = "interrupted"
	case strings.Contains(text, "unknown flag"), strings.Contains(text, "unknown option"), strings.Contains(text, "unrecognized argument"), strings.Contains(text, "unexpected argument"):
		d.Category = "invalid_arguments"
		d.Retryable = false
		d.RemoteCreationUncertain = false
	case strings.Contains(text, "not authenticated"), strings.Contains(text, "unauthorized"), strings.Contains(text, "login required"):
		d.Category = "authentication_required"
		d.Retryable = false
		d.RemoteCreationUncertain = false
	case strings.Contains(text, "permission denied"), strings.Contains(text, "forbidden"):
		d.Category = "authorization_denied"
		d.Retryable = false
		d.RemoteCreationUncertain = false
	case strings.Contains(text, "timed out"), strings.Contains(text, "timeout"):
		d.Category = "timeout"
	}
	return d
}
func (a *app) dispatchFailure(ctx context.Context, c common, id ids.ExecutionID, clientKey, profile string, cause error) *output.Error {
	d := &operationDiagnostic{Category: "upstream_failure", ExitCode: -1, Retryable: true, RemoteCreationUncertain: true}
	var classified *operationDiagnostic
	if errors.As(cause, &classified) {
		d = classified
	}
	problem := output.Wrap(output.CodeRemoteFailure, "create or recover Multica dispatch issue", d.Retryable, cause).
		WithDetail("client_key", clientKey).WithDetail("execution_id", id.String()).WithDetail("profile", profile).
		WithDetail("stage", "issue_create").WithDetail("diagnostic", d).
		WithDetail("recovery", "inspect status; retry the original dispatch with the same key and inputs after resolving the cause; never allocate a replacement key for an uncertain creation").
		WithActions(output.NextAction{Label: "Inspect dispatch and last operation failure", Argv: []string{"agentctl", "status", id.String()}, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	// The original operation may have been cancelled. Record only bounded metadata
	// with a fresh bounded context; inability to journal cannot mask the failure.
	recordCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	path, err := a.journalPath(c)
	if err != nil {
		return problem.WithDetail("diagnostic_recorded", false)
	}
	j := scopedJournal{path: path}
	for attempt := 0; attempt < 3; attempt++ {
		e, err := j.GetExecution(recordCtx, id)
		if err != nil || e.State.Terminal() {
			break
		}
		e.LastOperationFailure = &model.OperationFailure{Stage: "issue_create", Category: d.Category, UpstreamExitCode: d.ExitCode, Retryable: d.Retryable, RemoteCreationUncertain: d.RemoteCreationUncertain, RecordedAt: a.now().UTC()}
		_, err = j.UpdateExecution(recordCtx, e, e.Revision)
		if err == nil {
			return problem.WithDetail("diagnostic_recorded", true)
		}
	}
	return problem.WithDetail("diagnostic_recorded", false)
}
