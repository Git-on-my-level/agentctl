package anxrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
)

// RequeueResult is a local-only recovery receipt; dry runs report candidates
// in Requeued without changing files, locks, counters, or retention state.
type RequeueResult struct {
	DryRun           bool `json:"dry_run"`
	Requeued         int  `json:"requeued"`
	AlreadyPending   int  `json:"already_pending"`
	AlreadyDelivered int  `json:"already_delivered"`
	DeferredCapacity int  `json:"deferred_capacity"`
}

// RequeueDead preserves the registration, sequence and request-key hash. It
// resets delivery bookkeeping only, never evicts pending work, and never sends.
// Locks use Flush's order so concurrent workers cannot replay an entry twice.
// A crash between pending write and dead removal is safe: pending wins.
func (s *Spool) RequeueDead(ctx context.Context, dryRun bool) (RequeueResult, error) {
	result := RequeueResult{DryRun: dryRun}
	if !dryRun {
		release, err := s.flushLock(ctx)
		if err != nil {
			return result, err
		}
		defer release()
		unlock, err := s.lockContext(ctx, "state")
		if err != nil {
			return result, err
		}
		defer unlock()
	}
	dead, err := s.ListDead()
	if err != nil {
		return result, err
	}
	pending, err := s.ListPending()
	if err != nil {
		return result, err
	}
	index, err := s.index()
	if err != nil {
		return result, err
	}
	sort.Slice(dead, func(i, j int) bool {
		if !dead[i].CreatedAt.Equal(dead[j].CreatedAt) {
			return dead[i].CreatedAt.Before(dead[j].CreatedAt)
		}
		return entryKey(dead[i].ExecutionID, dead[i].Phase) < entryKey(dead[j].ExecutionID, dead[j].Phase)
	})
	occupied := map[string]bool{}
	for _, entry := range pending {
		occupied[entryKey(entry.ExecutionID, entry.Phase)] = true
	}
	count := len(pending)
	for _, entry := range dead {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		key := entryKey(entry.ExecutionID, entry.Phase)
		removeDead := false
		switch {
		case index.Delivered[replayKey(entry.Registration)] >= entry.Registration.Sequence && entry.Registration.Sequence > 0:
			result.AlreadyDelivered++
			removeDead = true
		case occupied[key]:
			result.AlreadyPending++
			removeDead = true
		case count >= s.maxPending:
			result.DeferredCapacity++
		default:
			// Preserve sequence zero legacy entries rather than inventing an identity.
			if entry.Registration.Sequence < 1 {
				return result, egressError(EgressFormat)
			}
			result.Requeued++
			count++
			occupied[key] = true
			if !dryRun {
				entry.Attempts, entry.ErrorCode, entry.LastError = 0, 0, ""
				entry.NextAttemptAt = s.now().UTC()
				entry.Revision++
				raw, err := json.Marshal(entry)
				if err != nil {
					return result, err
				}
				if err := writeFileAtomic(s.pendingPath(key), raw); err != nil {
					return result, err
				}
			}
			removeDead = true
		}
		if removeDead && !dryRun {
			if err := os.Remove(s.deadPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return result, err
			}
		}
	}
	return result, nil
}
