package main

import (
	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

// executionCaller accepts only a caller's explicit declaration. Neither the
// execution adapter nor native session environment is evidence of its caller.
// This is admission metadata, deliberately excluded from mutation digests.
func (a *app) executionCaller() (*model.ExecutionCaller, *output.Error) {
	if a.getenv == nil {
		return nil, nil
	}
	value := a.getenv(adapter.CallerHarnessEnv)
	if value == "" {
		return nil, nil
	}
	caller := &model.ExecutionCaller{Harness: model.CallerHarness(value), Provenance: model.CallerDeclared}
	if err := caller.Validate(); err != nil {
		// Do not echo arbitrary environment text: it may contain private data.
		return nil, output.NewError(output.CodeUsage, "AGENTCTL_CALLER_HARNESS must be one of hermes, claude-code, codex, cursor, omp, zcode, devin, other", false).
			WithDetail("diagnostic_code", "caller_harness_invalid")
	}
	return caller, nil
}
