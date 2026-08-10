package contracts

import (
	"context"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

// MutationKey provides local idempotency. InputDigest must cover all bounded
// semantic inputs; a repeated key with another digest is a conflict.
type MutationKey struct{ Scope, Key, InputDigest string }

func (k MutationKey) Enabled() bool { return k.Key != "" }

type EventQuery struct {
	AfterSequence uint64
	Limit         int
	Kinds         []model.EventKind
}

// Journal is the host-local operational authority. Event records are immutable;
// deletion is intentionally absent and belongs to explicit retention planning.
type Journal interface {
	HostID(context.Context) (ids.HostID, error)
	CreateExecution(context.Context, model.Execution, MutationKey) (model.Execution, bool, error)
	GetExecution(context.Context, ids.ExecutionID) (model.Execution, error)
	UpdateExecution(context.Context, model.Execution, uint64) (model.Execution, error)
	ListExecutions(context.Context, bool) ([]model.Execution, error)
	AppendEvent(context.Context, model.Event, []byte) (model.Event, bool, error)
	GetEvent(context.Context, ids.EventID) (model.Event, error)
	ListEvents(context.Context, ids.ExecutionID, EventQuery) ([]model.Event, error)
	Close() error
}
