package model

import (
	"errors"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

type UncollectedChild struct {
	ID           ids.ExecutionID       `json:"id"`
	CreatedAt    time.Time             `json:"created_at"`
	State        State                 `json:"state"`
	Authority    Authority             `json:"authority"`
	Adapter      string                `json:"adapter"`
	Remote       *DelegationRemotePlan `json:"remote,omitempty"`
	Reason       string                `json:"reason"`
	WaiterStatus string                `json:"waiter_status"`
	CollectArgv  []string              `json:"collect_argv"`
}

// ChildExitGuard records a bounded historical exit snapshot. A continuation is
// claimed before launch and is never retried automatically after owner loss.
type ChildExitGuard struct {
	CheckedAt      time.Time        `json:"checked_at"`
	CheckError     string           `json:"check_error,omitempty"`
	AutoContinue   string           `json:"auto_continue"`
	Diagnostic     string           `json:"diagnostic,omitempty"`
	ContinuationID *ids.ExecutionID `json:"continuation_id,omitempty"`
}

func validateUncollectedChildren(children []UncollectedChild) error {
	if len(children) > 200 {
		return errors.New("uncollected children exceed bound")
	}
	for _, child := range children {
		if _, err := ids.ParseExecutionID(child.ID.String()); err != nil {
			return err
		}
		if child.CreatedAt.IsZero() || !validState(child.State) || (child.Authority != AuthorityNative && child.Authority != AuthorityMultica) || !adapterPattern.MatchString(child.Adapter) {
			return errors.New("invalid uncollected child metadata")
		}
		if child.Reason != "terminal_uncollected" && child.Reason != "running_without_known_waiter" {
			return errors.New("invalid uncollected child reason")
		}
		if child.WaiterStatus != "unknown" && child.WaiterStatus != "not_applicable" {
			return errors.New("invalid child waiter status")
		}
		if len(child.CollectArgv) < 4 || len(child.CollectArgv) > 32 {
			return errors.New("invalid child collection argv")
		}
	}
	return nil
}

func (g ChildExitGuard) Validate() error {
	if g.CheckedAt.IsZero() || len(g.Diagnostic) > 128 || (g.CheckError != "" && g.CheckError != "children_query_incomplete") {
		return errors.New("invalid child exit guard")
	}
	switch g.AutoContinue {
	case "pending", "attempted", "launched", "failed", "disabled", "unavailable", "parent_not_completed", "parent_interrupted", "suppressed_recursive":
	default:
		return errors.New("invalid child exit continuation state")
	}
	if g.ContinuationID != nil {
		if _, err := ids.ParseExecutionID(g.ContinuationID.String()); err != nil {
			return err
		}
	}
	return nil
}
