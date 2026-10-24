package anxrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/route"
	"golang.org/x/sys/unix"
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
	Revision      int          `json:"revision"`
	ErrorCode     int          `json:"error_code"`
	LastError     string       `json:"last_error,omitempty"`
	Registration  Registration `json:"registration"`
}

// Sender delivers one registration. Implementations must be idempotent;
// delivery is at-least-once.
type Sender interface {
	Send(ctx context.Context, registration Registration) error
}

// Spool is the bounded local outbox. Processes serialize through file locks;
// network calls never hold the state lock needed by Enqueue.
type Spool struct {
	Catalog    []route.ModelRecord
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

// Independent file locks serialize local state mutations and entire flush
// passes. Network I/O never holds the state lock used by enqueue.
func (s *Spool) lock(name string, nonblocking bool) (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.root, name+".lock"), os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, err
	}
	flags := unix.LOCK_EX
	if nonblocking {
		flags |= unix.LOCK_NB
	}
	if err := unix.Flock(int(f.Fd()), flags); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

// Contending detached workers wait, then take their own fresh snapshot.
// A busy lock must never consume the wakeup for a newly enqueued terminal.
func (s *Spool) flushLock(ctx context.Context) (func(), error) {
	return s.lockContext(ctx, "flush")
}

func (s *Spool) lockContext(ctx context.Context, name string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := s.lock(name, true)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// LastRejection is a bounded, value-free receipt for a rejected registration.
type Rejection struct {
	Class string `json:"class"`
	Code  int    `json:"code"`
}

func (s *Spool) LastRejection() *Rejection {
	raw, err := os.ReadFile(filepath.Join(s.root, "egress-rejection.json"))
	var receipt Rejection
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Class != ReasonEgressRejected || validEgressCode(receipt.Code) == 0 {
		return nil
	}
	return &receipt
}

// Assigned identities are retained after pending/dead cleanup. Delivered is
// an explicit durable replay index; crashes before pending deletion are safe.
type sequenceIndex struct {
	Counters  map[string]int `json:"counters"`
	Assigned  map[string]int `json:"assigned"`
	Delivered map[string]int `json:"delivered"`
}

func (s *Spool) index() (sequenceIndex, error) {
	index := sequenceIndex{Counters: map[string]int{}, Assigned: map[string]int{}, Delivered: map[string]int{}}
	raw, err := os.ReadFile(filepath.Join(s.root, "sequence-index.json"))
	if os.IsNotExist(err) {
		// Preserve counters from the pre-v2 spool.
		if old, readErr := os.ReadFile(filepath.Join(s.root, "sequences.json")); readErr == nil {
			if err := json.Unmarshal(old, &index.Counters); err != nil {
				return index, err
			}
		} else if !os.IsNotExist(readErr) {
			return index, readErr
		}
		return index, nil
	}
	if err != nil {
		return index, err
	}
	if err = json.Unmarshal(raw, &index); err != nil {
		return index, err
	}
	if index.Counters == nil || index.Assigned == nil || index.Delivered == nil {
		return index, fmt.Errorf("invalid_sequence_index")
	}
	return index, nil
}

func (s *Spool) saveIndex(index sequenceIndex) error {
	raw, err := json.Marshal(index)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.root, "sequence-index.json"), raw)
}

func replayKey(r Registration) string { return r.DurableID + "/" + string(r.Phase) }

func (s *Spool) nextSequence(r Registration) (int, error) {
	index, err := s.index()
	if err != nil {
		return 0, err
	}
	key, scope := replayKey(r), sequenceScope(r)
	if seq := index.Assigned[key]; seq > 0 {
		return seq, nil
	}
	index.Counters[scope]++
	index.Assigned[key] = index.Counters[scope]
	return index.Assigned[key], s.saveIndex(index)
}

func (s *Spool) markDelivered(r Registration) error {
	index, err := s.index()
	if err != nil {
		return err
	}
	index.Delivered[replayKey(r)] = r.Sequence
	index.Assigned[replayKey(r)] = r.Sequence
	return s.saveIndex(index)
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
// native session is bound) from sequence-index.json.
func (s *Spool) Enqueue(registration Registration) (Entry, error) {
	unlock, err := s.lock("state", false)
	if err != nil {
		return Entry{}, err
	}
	defer unlock()
	if err := s.cleanup(0); err != nil {
		return Entry{}, err
	}
	if registration.RequestKey != "" {
		registration.RequestKeyHash, err = s.hashRequestKey(registration.RequestKey)
		if err != nil {
			return Entry{}, err
		}
	}
	registration, err = Allowlisted(registration, s.Catalog)
	if err != nil {
		class, code := ErrorClass(err)
		raw, _ := json.Marshal(Rejection{Class: class, Code: code})
		if writeErr := writeFileAtomic(filepath.Join(s.root, "egress-rejection.json"), raw); writeErr != nil {
			return Entry{}, writeErr
		}
		return Entry{}, err
	}
	key := entryKey(registration.ExternalID, registration.Phase)
	now := s.now().UTC()
	entry := Entry{SchemaVersion: 1, Revision: 1, ExecutionID: registration.ExternalID, Phase: registration.Phase, CreatedAt: now, NextAttemptAt: now, Registration: registration}
	if existing, err := s.read(s.pendingPath(key)); err == nil {
		entry.CreatedAt, entry.Attempts, entry.NextAttemptAt = existing.CreatedAt, existing.Attempts, existing.NextAttemptAt
		entry.LastError, entry.ErrorCode = existing.LastError, existing.ErrorCode
		entry.Registration.Sequence = existing.Registration.Sequence
		entry.Revision = existing.Revision + 1
	} else if errors.Is(err, os.ErrNotExist) {
		entry.Registration.Sequence, err = s.nextSequence(registration)
		if err != nil {
			return Entry{}, err
		}
		if err := s.evictOverflow(); err != nil {
			return Entry{}, err
		}
	} else {
		return Entry{}, err
	}
	// Also indexes old pending entries, whose sequences must survive delivery.
	index, err := s.index()
	if err != nil {
		return Entry{}, err
	}
	index.Assigned[replayKey(entry.Registration)] = entry.Registration.Sequence
	scope := sequenceScope(entry.Registration)
	if index.Counters[scope] < entry.Registration.Sequence {
		index.Counters[scope] = entry.Registration.Sequence
	}
	if err := s.saveIndex(index); err != nil {
		return Entry{}, err
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return Entry{}, err
	}
	if err := writeFileAtomic(s.pendingPath(key), encoded); err != nil {
		return Entry{}, err
	}
	if err := s.cleanup(0); err != nil {
		return Entry{}, err
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
	if _, err := ids.ParseExecutionID(entry.ExecutionID); err != nil {
		return Entry{}, fmt.Errorf("invalid_run_identity")
	}
	if entry.ExecutionID != entry.Registration.ExternalID || entry.Phase != entry.Registration.Phase {
		return Entry{}, fmt.Errorf("invalid_run_identity")
	}
	entry.Registration, err = Allowlisted(entry.Registration, s.Catalog)
	if err != nil {
		return Entry{}, err
	}
	entry.LastError = sanitizeReason(entry.LastError)
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
	result := FlushResult{}
	flushUnlock, err := s.flushLock(ctx)
	if err != nil {
		return result, err
	}
	held := true
	release := func() {
		if held {
			held = false
			flushUnlock()
		}
	}
	defer release()
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	unlock, err := s.lockContext(ctx, "state")
	if err != nil {
		return result, err
	}
	err = s.cleanup(0)
	pending, listErr := s.listPending()
	unlock()
	if err != nil {
		return result, err
	}
	if listErr != nil {
		return result, listErr
	}
	// Sequences, not timestamps or filenames, order session activity.
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].Registration.Sequence < pending[j].Registration.Sequence })
	for _, snapshot := range pending {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if snapshot.NextAttemptAt.After(s.now()) {
			result.SkippedNotDue++
			continue
		}
		// Revalidate legacy or edited entries at the last egress boundary.
		safe, sendErr := Allowlisted(snapshot.Registration, s.Catalog)
		if sendErr == nil {
			sendErr = sender.Send(ctx, safe)
		} else {
			sendErr = &SendError{Fatal: true, Reason: ReasonRejected}
		}
		interrupted := ctx.Err()
		if interrupted == nil && (errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded)) {
			interrupted = sendErr
		}
		if interrupted != nil {
			// The cleanup deadline governs the flush lock, not local retry
			// bookkeeping. Release before a best-effort, nonblocking state write.
			release()
			if unlock, err := s.lock("state", true); err == nil {
				_ = s.finishAttempt(snapshot, interrupted, maxAttempts, &result)
				_ = s.cleanup(0)
				unlock()
			}
			return result, interrupted
		}
		unlock, err := s.lockContext(ctx, "state")
		if err != nil {
			return result, err
		}
		err = s.finishAttempt(snapshot, sendErr, maxAttempts, &result)
		if err == nil {
			err = s.cleanup(0)
		}
		unlock()
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// Compare the entire snapshot as well as its revision: neither success nor
// retry/dead-letter may remove or overwrite a refreshed pending payload.
func (s *Spool) finishAttempt(snapshot Entry, sendErr error, maxAttempts int, result *FlushResult) error {
	key := entryKey(snapshot.ExecutionID, snapshot.Phase)
	current, err := s.read(s.pendingPath(key))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if sendErr == nil {
		if err := s.markDelivered(snapshot.Registration); err != nil {
			return err
		}
		result.Delivered++
	}
	if !reflect.DeepEqual(current, snapshot) {
		return nil
	}
	if sendErr == nil {
		return os.Remove(s.pendingPath(key))
	}
	current.Attempts++
	current.LastError, current.ErrorCode = ErrorClass(sendErr)
	if !retryableSendError(sendErr) || current.Attempts >= maxAttempts {
		if err := s.deadLetter(current, classifyDeadReason(sendErr)); err != nil {
			return err
		}
		result.DeadLettered++
		return nil
	}
	current.NextAttemptAt = s.now().UTC().Add(backoff(current.Attempts))
	raw, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.pendingPath(key), raw); err != nil {
		return err
	}
	result.Retried++
	return nil
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
	unlock, err := s.lock("state", false)
	if err != nil {
		return err
	}
	defer unlock()
	return s.cleanup(retention)
}

func (s *Spool) cleanup(retention time.Duration) error {
	if retention <= 0 {
		retention = DefaultRetention
	}
	dead, err := s.ListDead()
	if err != nil {
		return err
	}
	cutoff := s.now().Add(-retention)
	for i, entry := range dead {
		if entry.CreatedAt.After(cutoff) && i >= len(dead)-DefaultMaxPending {
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
	switch value {
	case "":
		return ""
	case ReasonAuthRequired, ReasonRejected, ReasonCardUnresolved, ReasonConflict,
		ReasonEgressRejected, "spool_full", "anx_delivery_failed", "anx_unavailable", "anx_timeout", "anx_protocol_error", "local_io":
		return value
	default:
		return "anx_delivery_failed"
	}
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
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
