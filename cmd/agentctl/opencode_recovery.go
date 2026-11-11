package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func verifiedOpenCodeExecution(execution model.Execution) bool {
	version := execution.Capabilities.BackendVersion
	return version != nil && adapter.OpenCodeResumeVersionSupported(*version)
}

// Export is a native existence check, not an imported conversation. OpenCode
// 2.2.6 exits nonzero for missing sessions, whereas run --session creates them.
// Never run this during a plan, capture its transcript, or expose native errors.
func verifyOpenCodeSession(ctx context.Context, opts runOptions) *output.Error {
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, opts.argv[0], "session", "export", "--sanitize", "--", opts.resumeSession)
	cmd.Dir = opts.cwd
	cmd.Env = adapter.WithoutCallerDeclaration(append(os.Environ(), opts.env...))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return interruptedRunError(ctx.Err(), model.Execution{})
		}
		return output.NewError(output.CodeCapabilityUnavailable, "OpenCode could not verify the recorded native session; no continuation prompt was sent", false).
			WithDetail("diagnostic_code", "continue_opencode_session_unverified")
	}
	return nil
}
