package anxrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The spool is a bounded, owner-only, file-backed outbox beside the agentctl
// journal state. One entry exists per (execution, phase) pair, so re-enqueueing
// a phase is an idempotent overwrite and delivery is at-least-once with
// consumer-side dedupe (ANX upserts are idempotent on identity + sequence).

const (
	// DefaultMaxPending bounds the pending outbox. The oldest entries are
	// dead-lettered with reason spool_full rather than dropped silently.
	DefaultMaxPending = 256
	// DefaultMaxAttempts dead-letters an entry after this many attempts.
	DefaultMaxAttempts = 8
	// DefaultRetention bounds how long a dead-lettered entry is kept.
	DefaultRetention = 7 * 24 * time.Hour
	// initialBackoff and maxBackoff bound the retry schedule.
	initialBackoff = 30 * time.Second
	maxBackoff     = 1 * time.Hour

	dirPerm  = 0o700
	filePerm = 0o600
)

// Spool errors are wrapped with the entry id; registration payloads and
// summaries never enter error strings.
var (
	ErrSpoolClosed = errors.New("anx spool closed")
)

// Entry is one durable pending registration.
type Entry struct {
	SchemaVersion int          `json:"schema_version"`
	ExecutionID   string       `json:"execution_id"`
	Phase         Phase        `json:"phase"`
	CreatedAt     time.Time    `json:"created_at"`
	Attempts      int          `json:"attempts"`
	NextAttemptAt time.Time    `json:"next_attempt_at"`
	LastError     string       `json:"last_error,omitempty"`
	Registration  Registration `json:"registration"`
}

// Sender delivers one registration. Implementations must be idempotent;
// delivery is at-least-once.
type Sender interface {
	Send(ctx context.Context, registration Registration) error
}

// Spool is the bounded local outbox. All methods are safe for one process;
// concurrent processes serialize on the pending-file rename dance below.
type Spool struct {
	root       string
	maxPending int
	now        func() time.Time
}

// OpenSpool prepares (and creates if needed) the owner-only spool under root.
func OpenSpool(root string) (*Spool, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("anx spool root is required")
	}
	clean, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("resolve anx spool root: %w", err)
	}
	for _, dir := range []string{clean, filepath.Join(clean, "pending"), filepath.Join(clean, "dead")} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("prepare anx spool directory: %w", err)
		}
		if err := enforceOwnerDir(dir); err != nil {
			return nil, err
		}
	}
	return &Spool{root: clean, maxPending: DefaultMaxPending, now: time.Now}, nil
}

// OpenSpoolIfPresent opens an existing spool without creating anything, so
// read-only status reporting never mutates the filesystem. ok is false when
// the spool does not exist yet.
func OpenSpoolIfPresent(root string) (spool *Spool, ok bool, err error) {
	if strings.TrimSpace(root) == "" {
		return nil, false, fmt.Errorf("anx spool root is required")
	}
	clean, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, false, fmt.Errorf("resolve anx spool root: %w", err)
	}
	for _, dir := range []string{clean, filepath.Join(clean, "pending"), filepath.Join(clean, "dead")} {
		if err := enforceOwnerDir(dir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, false, nil
			}
			return nil, false, err
		}
	}
	return &Spool{root: clean, maxPending: DefaultMaxPending, now: time.Now}, true, nil
}

// Root reports the spool root (for status output).
func (s *Spool) Root() string { return s.root }

func enforceOwnerDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("anx spool path %s is not a directory", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("anx spool directory %s must be 0700 or stricter", dir)
	}
	return nil
}

func entryKey(executionID string, phase Phase) string {
	return fmt.Sprintf("%s.%s.json", executionID, phase)
}

// sequenceScope keys the monotonic sequence counter: the native session
// fingerprint when one is bound (it spans executions), else the execution.
func sequenceScope(registration Registration) string {
	if registration.NativeSession != nil && registration.NativeSession.Fingerprint != "" {
		return registration.NativeSession.Fingerprint
	}
	return registration.ExternalID
}

// nextSequence returns and durably increments the counter for one scope.
func (s *Spool) nextSequence(scope string) (int, error) {
	path := filepath.Join(s.root, "sequences.json")
	counters := map[string]int{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &counters); err != nil {
			return 0, fmt.Errorf("decode anx spool sequence counters: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	next := counters[scope] + 1
	counters[scope] = next
	encoded, err := json.Marshal(counters)
	if err != nil {
		return 0, err
	}
	if err := writeFileAtomic(path, encoded); err != nil {
		return 0, fmt.Errorf("write anx spool sequence counters: %w", err)
	}
	return next, nil
}

func (s *Spool) pendingPath(key string) string {
	return filepath.Join(s.root, "pending", key)
}

func (s *Spool) deadPath(key string) string {
	return filepath.Join(s.root, "dead", key)
}

// Enqueue spools one registration, idempotently keyed by (execution, phase).
// A re-enqueue of the same phase overwrites the payload but preserves the
// original created_at, attempt history, and assigned sequence, so terminal
// refreshes never reset backoff or replay protection. New entries draw the
// next monotonic sequence for their native session (or execution when no
// native session is bound) from sequences.json.
func (s *Spool) Enqueue(registration Registration) (Entry, error) {
	if registration.SchemaVersion != RegistrationSchemaVersion || registration.ExternalID == "" || registration.Phase == "" {
		return Entry{}, fmt.Errorf("registration identity is incomplete")
	}
	key := entryKey(registration.ExternalID, registration.Phase)
	now := s.now().UTC()
	entry := Entry{SchemaVersion: 1, ExecutionID: registration.ExternalID, Phase: registration.Phase, CreatedAt: now, NextAttemptAt: now, Registration: registration}
	if existing, err := s.read(s.pendingPath(key)); err == nil {
		entry.CreatedAt = existing.CreatedAt
		entry.Attempts = existing.Attempts
		entry.NextAttemptAt = existing.NextAttemptAt
		entry.LastError = existing.LastError
		entry.Registration.Sequence = existing.Registration.Sequence
	} else if errors.Is(err, os.ErrNotExist) {
		sequence, seqErr := s.nextSequence(sequenceScope(registration))
		if seqErr != nil {
			return Entry{}, seqErr
		}
		entry.Registration.Sequence = sequence
	} else {
		return Entry{}, err
	}
	if err := s.evictOverflow(); err != nil {
		return Entry{}, err
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return Entry{}, fmt.Errorf("encode anx spool entry: %w", err)
	}
	if err := writeFileAtomic(s.pendingPath(key), encoded); err != nil {
		return Entry{}, fmt.Errorf("write anx spool entry: %w", err)
	}
	return entry, nil
}

// evictOverflow dead-letters the oldest pending entries so a new enqueue
// still fits inside maxPending.
func (s *Spool) evictOverflow() error {
	pending, err := s.listPending()
	if err != nil || len(pending) < s.maxPending {
		return err
	}
	overflow := len(pending) + 1 - s.maxPending
	for _, entry := range pending[:overflow] {
		if err := s.deadLetter(entry, "spool_full"); err != nil {
			return err
		}
	}
	return nil
}

// ListPending returns pending entries oldest-first.
func (s *Spool) ListPending() ([]Entry, error) {
	return s.listPending()
}

func (s *Spool) listPending() ([]Entry, error) {
	matches, err := filepath.Glob(filepath.Join(s.root, "pending", "*.json"))
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(matches))
	for _, path := range matches {
		entry, err := s.read(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, k int) bool {
		if !entries[i].CreatedAt.Equal(entries[k].CreatedAt) {
			return entries[i].CreatedAt.Before(entries[k].CreatedAt)
		}
		return entries[i].ExecutionID < entries[k].ExecutionID
	})
	return entries, nil
}

// ListDead returns dead-lettered entries oldest-first.
func (s *Spool) ListDead() ([]Entry, error) {
	matches, err := filepath.Glob(filepath.Join(s.root, "dead", "*.json"))
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(matches))
	for _, path := range matches {
		entry, err := s.read(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, k int) bool {
		return entries[i].CreatedAt.Before(entries[k].CreatedAt)
	})
	return entries, nil
}

func (s *Spool) read(path string) (Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var entry Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return Entry{}, fmt.Errorf("decode anx spool entry %s: %w", filepath.Base(path), err)
	}
	if entry.SchemaVersion != 1 || entry.ExecutionID == "" || entry.Phase == "" {
		return Entry{}, fmt.Errorf("malformed anx spool entry %s", filepath.Base(path))
	}
	return entry, nil
}

// FlushResult summarizes one flush pass.
type FlushResult struct {
	Delivered     int
	Retried       int
	DeadLettered  int
	SkippedNotDue int
}

// Flush delivers every due pending entry through sender. Never-fatal by
// contract: per-entry failures schedule a retry (backoff) or dead-letter; the
// returned result reports what happened. Entries whose NextAttemptAt is in
// the future are skipped.
func (s *Spool) Flush(ctx context.Context, sender Sender, maxAttempts int) (FlushResult, error) {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	result := FlushResult{}
	pending, err := s.listPending()
	if err != nil {
		return result, err
	}
	for _, entry := range pending {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if !entry.NextAttemptAt.IsZero() && entry.NextAttemptAt.After(s.now()) {
			result.SkippedNotDue++
			continue
		}
		key := entryKey(entry.ExecutionID, entry.Phase)
		sendErr := sender.Send(ctx, entry.Registration)
		if sendErr == nil {
			if err := os.Remove(s.pendingPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return result, fmt.Errorf("remove delivered anx spool entry: %w", err)
			}
			result.Delivered++
			continue
		}
		entry.Attempts++
		entry.LastError = sanitizeReason(sendErr.Error())
		if !retryableSendError(sendErr) || entry.Attempts >= maxAttempts {
			if err := s.deadLetter(entry, classifyDeadReason(sendErr)); err != nil {
				return result, err
			}
			result.DeadLettered++
			continue
		}
		entry.NextAttemptAt = s.now().UTC().Add(backoff(entry.Attempts))
		encoded, err := json.Marshal(entry)
		if err != nil {
			return result, fmt.Errorf("encode anx spool entry: %w", err)
		}
		if err := writeFileAtomic(s.pendingPath(key), encoded); err != nil {
			return result, fmt.Errorf("rewrite anx spool entry: %w", err)
		}
		result.Retried++
	}
	return result, nil
}

// deadLetter moves an entry to dead/ with a reason.
func (s *Spool) deadLetter(entry Entry, reason string) error {
	entry.LastError = sanitizeReason(reason)
	return s.deadLetterWithAttempts(entry, reason)
}

func (s *Spool) deadLetterWithAttempts(entry Entry, reason string) error {
	key := entryKey(entry.ExecutionID, entry.Phase)
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode anx spool dead letter: %w", err)
	}
	if err := writeFileAtomic(s.deadPath(key), encoded); err != nil {
		return fmt.Errorf("write anx spool dead letter: %w", err)
	}
	if err := os.Remove(s.pendingPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove dead-lettered anx spool entry: %w", err)
	}
	return nil
}

// Cleanup drops dead letters older than retention.
func (s *Spool) Cleanup(retention time.Duration) error {
	if retention <= 0 {
		retention = DefaultRetention
	}
	dead, err := s.ListDead()
	if err != nil {
		return err
	}
	cutoff := s.now().Add(-retention)
	for _, entry := range dead {
		if entry.CreatedAt.After(cutoff) {
			continue
		}
		if err := os.Remove(s.deadPath(entryKey(entry.ExecutionID, entry.Phase))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func backoff(attempts int) time.Duration {
	delay := initialBackoff
	for range attempts - 1 {
		delay *= 2
		if delay >= maxBackoff {
			return maxBackoff
		}
	}
	return delay
}

func sanitizeReason(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 256 {
		value = value[:256]
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, value)
}

// writeFileAtomic writes via a same-directory temp file and rename so a crash
// never leaves a half-written spool entry.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temp.Chmod(filePerm); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
