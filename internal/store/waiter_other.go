//go:build !darwin && !linux

package store

import (
	"context"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

// Unsupported lock platforms remain conservative: no live waiter evidence.
type Waiter struct{}

func (*Waiter) Close() error                                                   { return nil }
func RegisterWaiter(context.Context, string, ids.ExecutionID) (*Waiter, error) { return nil, nil }
func hasLiveWaiter(context.Context, string, ids.ExecutionID) (bool, error)     { return false, nil }
