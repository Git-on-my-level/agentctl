//go:build !darwin && !linux

package store

import (
	"context"
	"github.com/Git-on-my-level/agentctl/internal/ids"
)

type Waiter struct{}

func (*Waiter) Close() error { return nil }

// Other platforms retain conservative unknown waiter coverage.
func RegisterWaiter(string, ids.ExecutionID) (*Waiter, error)              { return nil, nil }
func hasLiveWaiter(context.Context, string, ids.ExecutionID) (bool, error) { return false, nil }
