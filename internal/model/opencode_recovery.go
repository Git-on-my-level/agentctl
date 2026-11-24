package model

import (
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

type OpenCodeRecoveryKind string

const (
	OpenCodeRateLimit   OpenCodeRecoveryKind = "rate_limit"
	OpenCodeFinalAnswer OpenCodeRecoveryKind = "final_answer"
	OpenCodeRetryLimit                       = 3
)

func (k OpenCodeRecoveryKind) validAttempt(n int) bool {
	return k == OpenCodeRateLimit && n >= 1 && n <= OpenCodeRetryLimit ||
		k == OpenCodeFinalAnswer && n == 1
}

// OpenCodeRecovery takes effect only after the physical turn has exited.
// Claiming it does not mean a prompt was sent. The journal links the successor
// at admission, and an expired or closed claim can never admit a successor.
type OpenCodeRecovery struct {
	Kind      OpenCodeRecoveryKind `json:"kind"`
	Attempt   int                  `json:"attempt"`
	NotBefore time.Time            `json:"not_before"`
	ExpiresAt time.Time            `json:"expires_at"`
	Claimed   bool                 `json:"claimed"`
	Closed    bool                 `json:"closed"`
}

func (e Execution) OpenCodeRecoveryPending(now time.Time) bool {
	r := e.OpenCodeRecovery
	return r != nil && !r.Closed && e.SupersededBy == nil && now.Before(r.ExpiresAt)
}

func ValidOpenCodeRecoveryChain(chain []ids.ExecutionID, original ids.ExecutionID) bool {
	if len(chain) == 0 {
		return true
	}
	if chain[0] != original || len(chain) > OpenCodeRetryLimit+2 {
		return false
	}
	seen := make(map[ids.ExecutionID]bool, len(chain))
	for _, id := range chain {
		if _, err := ids.ParseExecutionID(id.String()); err != nil || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
