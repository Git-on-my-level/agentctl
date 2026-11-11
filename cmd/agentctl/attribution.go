package main

import (
	"context"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func (a *app) launchAttribution(ctx context.Context, c common) (*ids.ExecutionID, *model.CallerIdentity, *output.Error) {
	report, problem := a.resolveCallerIdentity(ctx, c, "", "", "")
	if problem != nil {
		return nil, nil, problem
	}
	var parent *ids.ExecutionID
	// Only journal-observed execution evidence binds a local parent. Inherited
	// conflicting markers have already been rejected by identity's shared rules.
	if report.Execution.ID != nil && report.Execution.Confidence == "observed" {
		id, _ := ids.ParseExecutionID(*report.Execution.ID)
		parent = &id
	}
	var caller *model.CallerIdentity
	if report.Provider.ID != nil && report.NativeSession.ID != nil {
		caller = &model.CallerIdentity{Provider: *report.Provider.ID, NativeSessionID: *report.NativeSession.ID, Provenance: report.NativeSession.Provenance}
	}
	return parent, caller, nil
}
