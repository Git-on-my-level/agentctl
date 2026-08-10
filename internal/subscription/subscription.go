// Package subscription defines storage-neutral event stream filters, cursor
// binding, and two-phase subscription rotation.
package subscription

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/callback"
)

var (
	ErrInvalidFilter   = errors.New("invalid subscription filter")
	ErrCursorExpired   = errors.New("cursor expired")
	ErrCursorMismatch  = errors.New("cursor does not match origin or filter")
	ErrNotDurable      = errors.New("replacement subscription is not durable")
	ErrInvalidRotation = errors.New("invalid subscription rotation")
)

// EventFilter is intentionally explicit: labels and broad unscoped queries
// cannot be used to bind a cursor or subscription.
type EventFilter struct {
	ExecutionIDs []string          `json:"execution_ids,omitempty"`
	Authority    string            `json:"authority,omitempty"`
	Scope        map[string]string `json:"scope,omitempty"`
	Kinds        []string          `json:"kinds,omitempty"`
	Query        string            `json:"query,omitempty"`
}

func (f EventFilter) Normalize() (EventFilter, error) {
	if len(f.ExecutionIDs) == 0 && strings.TrimSpace(f.Authority) == "" && strings.TrimSpace(f.Query) == "" {
		return EventFilter{}, fmt.Errorf("%w: exact execution, authority, or narrow query required", ErrInvalidFilter)
	}
	result := EventFilter{Authority: strings.TrimSpace(f.Authority), Query: strings.TrimSpace(f.Query)}
	seen := make(map[string]struct{})
	for _, id := range f.ExecutionIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return EventFilter{}, fmt.Errorf("%w: empty execution ID", ErrInvalidFilter)
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			result.ExecutionIDs = append(result.ExecutionIDs, id)
		}
	}
	sort.Strings(result.ExecutionIDs)
	seen = make(map[string]struct{})
	for _, kind := range f.Kinds {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			return EventFilter{}, fmt.Errorf("%w: empty event kind", ErrInvalidFilter)
		}
		if _, ok := seen[kind]; !ok {
			seen[kind] = struct{}{}
			result.Kinds = append(result.Kinds, kind)
		}
	}
	sort.Strings(result.Kinds)
	if len(f.Scope) > 0 {
		result.Scope = make(map[string]string, len(f.Scope))
		for k, v := range f.Scope {
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if k == "" || v == "" {
				return EventFilter{}, fmt.Errorf("%w: scope keys and values cannot be empty", ErrInvalidFilter)
			}
			result.Scope[k] = v
		}
	}
	if result.Query != "" && result.Authority == "" {
		return EventFilter{}, fmt.Errorf("%w: query must bind an authority", ErrInvalidFilter)
	}
	return result, nil
}

// Digest is the hash of the normalized filter, used as an immutable cursor
// binding. It deliberately includes all target/scope fields.
func (f EventFilter) Digest() (string, error) {
	normalized, err := f.Normalize()
	if err != nil {
		return "", err
	}
	b, err := callback.CanonicalJSON(normalized)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

// Cursor is an immutable checkpoint. ID allocation/storage are intentionally
// outside this package; all fields that affect replay are represented here.
type Cursor struct {
	ID           string    `json:"id"`
	Origin       string    `json:"origin_host_id"`
	Stream       string    `json:"stream_scope"`
	Sequence     uint64    `json:"sequence"`
	EventID      string    `json:"event_id"`
	EventHash    string    `json:"event_hash"`
	FilterDigest string    `json:"filter_digest"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (c Cursor) Validate(now time.Time, origin, stream string, filter EventFilter) error {
	if c.ID == "" || c.Origin == "" || c.Stream == "" || c.Sequence == 0 || c.EventID == "" || c.EventHash == "" || c.FilterDigest == "" {
		return fmt.Errorf("%w: malformed cursor", ErrCursorMismatch)
	}
	if now.IsZero() {
		now = time.Now()
	}
	if !c.ExpiresAt.IsZero() && now.After(c.ExpiresAt) {
		return ErrCursorExpired
	}
	digest, err := filter.Digest()
	if err != nil {
		return err
	}
	if c.Origin != origin || c.Stream != stream || c.FilterDigest != digest {
		return ErrCursorMismatch
	}
	return nil
}

// NewCursor binds a checkpoint to the exact normalized filter.
func NewCursor(id, origin, stream string, sequence uint64, eventID, eventHash string, filter EventFilter, created, expires time.Time) (Cursor, error) {
	if id == "" || origin == "" || stream == "" || sequence == 0 || eventID == "" || eventHash == "" {
		return Cursor{}, fmt.Errorf("%w: missing checkpoint field", ErrCursorMismatch)
	}
	digest, err := filter.Digest()
	if err != nil {
		return Cursor{}, err
	}
	if expires.Before(created) {
		return Cursor{}, fmt.Errorf("%w: expiration precedes creation", ErrCursorMismatch)
	}
	return Cursor{ID: id, Origin: origin, Stream: stream, Sequence: sequence, EventID: eventID, EventHash: eventHash, FilterDigest: digest, CreatedAt: created.UTC(), ExpiresAt: expires.UTC()}, nil
}

// Subscription binds an event filter to a destination. The destination is
// represented by callback.Destination, while persistence remains caller-owned.
type Subscription struct {
	ID                     string               `json:"id"`
	Origin                 string               `json:"origin_host_id"`
	Filter                 EventFilter          `json:"filter"`
	Destination            callback.Destination `json:"destination"`
	ExpiresAt              time.Time            `json:"expires_at"`
	CoordinatorExecutionID string               `json:"coordinator_execution_id,omitempty"`
	Cursor                 Cursor               `json:"cursor"`
	Replaces               string               `json:"replaces,omitempty"`
	ReplacedBy             string               `json:"replaced_by,omitempty"`
	State                  State                `json:"state"`
	AutoExpireOnTerminal   bool                 `json:"auto_expire_on_terminal"`
	ResolvedAuthority      string               `json:"resolved_authority,omitempty"`
	ResolvedScope          map[string]string    `json:"resolved_scope,omitempty"`
}

type State string

const (
	StateActive    State = "active"
	StatePaused    State = "paused"
	StateCancelled State = "cancelled"
	StateStopped   State = "stopped"
)

func (s Subscription) Validate(now time.Time) error {
	if s.ID == "" || s.Origin == "" || s.State == "" {
		return errors.New("subscription ID, origin, and state are required")
	}
	if _, err := s.Filter.Normalize(); err != nil {
		return err
	}
	if err := s.Destination.Validate(); err != nil {
		return err
	}
	if !s.ExpiresAt.IsZero() && !now.IsZero() && now.After(s.ExpiresAt) && s.State == StateActive {
		return errors.New("active subscription is expired")
	}
	return nil
}

// Rotation is the two-phase handoff record. Old remains active through phase
// one; commit stops it only after NewDurable is proven.
type Rotation struct {
	OldID      string `json:"old_id"`
	NewID      string `json:"new_id"`
	NewDurable bool   `json:"new_durable"`
	Committed  bool   `json:"committed"`
}

func PrepareRotation(old Subscription, replacement Subscription) (Rotation, Subscription, error) {
	if old.ID == "" || replacement.ID == "" || old.ID == replacement.ID {
		return Rotation{}, Subscription{}, ErrInvalidRotation
	}
	if replacement.Replaces != "" && replacement.Replaces != old.ID {
		return Rotation{}, Subscription{}, ErrInvalidRotation
	}
	replacement.Replaces = old.ID
	// The old record is not mutated by preparation. The caller persists this
	// replacement before calling CommitRotation.
	return Rotation{OldID: old.ID, NewID: replacement.ID}, replacement, nil
}

func CommitRotation(old *Subscription, replacement *Subscription, durable bool) (Rotation, error) {
	if old == nil || replacement == nil || old.ID == "" || replacement.ID == "" || old.ID == replacement.ID {
		return Rotation{}, ErrInvalidRotation
	}
	if !durable {
		return Rotation{OldID: old.ID, NewID: replacement.ID, NewDurable: false}, ErrNotDurable
	}
	replacement.Replaces = old.ID
	old.ReplacedBy = replacement.ID
	old.State = StateStopped
	if replacement.State == "" {
		replacement.State = StateActive
	}
	return Rotation{OldID: old.ID, NewID: replacement.ID, NewDurable: true, Committed: true}, nil
}

// Rotate is a convenience for callers that have already durably persisted
// replacement. It leaves old untouched when durable is false.
func Rotate(old Subscription, replacement Subscription, durable bool) (Subscription, Subscription, Rotation, error) {
	rotation, replacement, err := PrepareRotation(old, replacement)
	if err != nil {
		return Subscription{}, Subscription{}, Rotation{}, err
	}
	if !durable {
		return old, replacement, rotation, ErrNotDurable
	}
	oldCopy := old
	newCopy := replacement
	rotation, err = CommitRotation(&oldCopy, &newCopy, true)
	if err != nil {
		return Subscription{}, Subscription{}, Rotation{}, err
	}
	return oldCopy, newCopy, rotation, nil
}

// Registry gives an in-memory idempotent reference implementation. The
// journal/store integration can use the same semantics with transactions.
type Registry struct {
	mu    sync.Mutex
	items map[string]Subscription
}

func NewRegistry() *Registry { return &Registry{items: make(map[string]Subscription)} }

func (r *Registry) Put(s Subscription) error {
	if r == nil {
		return errors.New("nil subscription registry")
	}
	if err := s.Validate(time.Time{}); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.items[s.ID]; ok && !reflect.DeepEqual(existing, s) {
		return fmt.Errorf("subscription %q already exists", s.ID)
	}
	r.items[s.ID] = s
	return nil
}

func (r *Registry) Get(id string) (Subscription, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.items[id]
	return s, ok
}
