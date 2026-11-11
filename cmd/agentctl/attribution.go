package main

import (
	"context"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func (a *app) launchAttribution(ctx context.Context, c common, borrowed ...*store.Journal) (*ids.ExecutionID, *model.CallerIdentity, *output.Error) {
	option := identityResolutionOptions{failUnavailable: true}
	if len(borrowed) != 0 {
		option.journal = borrowed[0]
	}
	report, problem := a.resolveCallerIdentity(ctx, c, "", "", "", option)
	if problem != nil {
		if _, present := problem.Details["diagnostic_code"]; !present {
			problem.WithDetail("diagnostic_code", "caller_identity_unavailable")
		}
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
