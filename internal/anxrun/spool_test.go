package anxrun

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

func testSpool(t *testing.T) *Spool {
	t.Helper()
	spool, err := OpenSpool(filepath.Join(t.TempDir(), "anx-run-links"))
	if err != nil {
		t.Fatal(err)
	}
	return spool
}

func testRegistration(executionID string, phase Phase, sequence int) Registration {
	executionID = fixtureExecID(executionID)
	return Registration{SchemaVersion: RegistrationSchemaVersion, Phase: phase, Launcher: "agentctl", ExternalID: executionID, OriginHostID: "host-goose-addict-spray-hawk-lens-horror", DurableID: "agentctl://host-goose-addict-spray-hawk-lens-horror/" + executionID, State: StateRunning, Liveness: LivenessAlive, Labels: []string{"anx.card.watch-redesign"}, CardRefs: []string{"card:watch-redesign"}, Sequence: sequence,
		NativeSession: &NativeSession{Provider: "claude", Kind: NativeSessionIDKind, Fingerprint: "sha256:" + fmt.Sprintf("%064d", 7)}}
}

func fixtureExecID(value string) string {
	if _, err := ids.ParseExecutionID(value); err == nil {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	id, _ := ids.FromPayload(ids.TypeExecution, binary.BigEndian.Uint64(digest[:8])&((1<<60)-1))
	return id.String()
}

// recordingSender fakes delivery outcomes per attempt.
type recordingSender struct {
	calls     []Registration
	failures  int // fail the first N calls
	fatalOnce bool
}

func (s *recordingSender) Send(_ context.Context, registration Registration) error {
	s.calls = append(s.calls, registration)
	if len(s.calls) <= s.failures {
		if s.fatalOnce {
			return &SendError{Fatal: true, Reason: ReasonCardUnresolved, Detail: "card missing"}
		}
		return &SendError{Detail: "anx unreachable"}
	}
	return nil
}

func TestSpoolEnqueueIsIdempotentPerExecutionPhase(t *testing.T) {
	spool := testSpool(t)
	first, err := spool.Enqueue(testRegistration("exec-a", PhaseStart, 0))
	if err != nil {
		t.Fatal(err)
	}
	// A second enqueue of the same phase refreshes the payload but keeps the
	// original created_at and sequence: delivery stays replay-safe.
	later := testRegistration("exec-a", PhaseStart, 99)
	later.State = StateRunning
	second, err := spool.Enqueue(later)
	if err != nil {
		t.Fatal(err)
	}
	if second.Registration.Sequence != first.Registration.Sequence {
		t.Fatalf("sequence reassigned on re-enqueue: %d vs %d", second.Registration.Sequence, first.Registration.Sequence)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("created_at reset on re-enqueue")
	}
	pending, err := spool.ListPending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %#v err=%v", pending, err)
	}
	// A different phase is a separate entry.
	if _, err := spool.Enqueue(testRegistration("exec-a", PhaseTerminal, 0)); err != nil {
		t.Fatal(err)
	}
	if pending, _ := spool.ListPending(); len(pending) != 2 {
		t.Fatalf("phases did not create separate entries: %#v", pending)
	}
}

func TestSpoolSequenceMonotonicPerSession(t *testing.T) {
	spool := testSpool(t)
	first, err := spool.Enqueue(testRegistration("exec-a", PhaseStart, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Same native session fingerprint (follow-up turn), new execution.
	next := testRegistration("exec-b", PhaseStart, 0)
	second, err := spool.Enqueue(next)
	if err != nil {
		t.Fatal(err)
	}
	if second.Registration.Sequence <= first.Registration.Sequence {
		t.Fatalf("sequence not monotonic per session: %d then %d", first.Registration.Sequence, second.Registration.Sequence)
	}
	// Different fingerprint starts its own counter.
	other := testRegistration("exec-c", PhaseStart, 0)
	other.NativeSession.Fingerprint = "sha256:" + fmt.Sprintf("%064d", 9)
	third, err := spool.Enqueue(other)
	if err != nil {
		t.Fatal(err)
	}
	if third.Registration.Sequence != 1 {
		t.Fatalf("independent session sequence = %d, want 1", third.Registration.Sequence)
	}
}

func TestSpoolFlushDeliversAndRemoves(t *testing.T) {
	spool := testSpool(t)
	if _, err := spool.Enqueue(testRegistration("exec-a", PhaseTerminal, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Enqueue(testRegistration("exec-b", PhaseTerminal, 0)); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	result, err := spool.Flush(context.Background(), sender, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Delivered != 2 || len(sender.calls) != 2 {
		t.Fatalf("result = %#v calls = %d", result, len(sender.calls))
	}
	if pending, _ := spool.ListPending(); len(pending) != 0 {
		t.Fatalf("delivered entries not removed: %#v", pending)
	}
	// Flushing again is a no-op (at-least-once, consumer dedupes).
	if result, _ := spool.Flush(context.Background(), sender, 0); result.Delivered != 0 {
		t.Fatalf("second flush delivered again: %#v", result)
	}
}

func TestSpoolFlushRetriesWithBackoff(t *testing.T) {
	spool := testSpool(t)
	now := time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC)
	spool.now = func() time.Time { return now }
	if _, err := spool.Enqueue(testRegistration("exec-a", PhaseTerminal, 0)); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{failures: 1}
	result, err := spool.Flush(context.Background(), sender, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Retried != 1 || result.Delivered != 0 {
		t.Fatalf("result = %#v", result)
	}
	pending, _ := spool.ListPending()
	if len(pending) != 1 || pending[0].Attempts != 1 || pending[0].LastError == "" {
		t.Fatalf("retry state wrong: %#v", pending)
	}
	if pending[0].NextAttemptAt.Before(now.Add(initialBackoff)) {
		t.Fatalf("backoff too short: %v", pending[0].NextAttemptAt)
	}
	// Not due yet: skipped. After the deadline, it delivers.
	if result, _ := spool.Flush(context.Background(), sender, 0); result.SkippedNotDue != 1 {
		t.Fatalf("not-due entry was not skipped: %#v", result)
	}
	spool.now = func() time.Time { return now.Add(2 * initialBackoff) }
	if result, _ := spool.Flush(context.Background(), sender, 0); result.Delivered != 1 {
		t.Fatalf("due entry not delivered: %#v", result)
	}
}

func TestSpoolFlushDeadLettersFatalErrors(t *testing.T) {
	spool := testSpool(t)
	if _, err := spool.Enqueue(testRegistration("exec-a", PhaseTerminal, 0)); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{failures: 1, fatalOnce: true}
	result, err := spool.Flush(context.Background(), sender, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeadLettered != 1 || result.Retried != 0 {
		t.Fatalf("result = %#v", result)
	}
	dead, _ := spool.ListDead()
	if len(dead) != 1 || dead[0].LastError != ReasonCardUnresolved {
		t.Fatalf("dead letter wrong: %#v", dead)
	}
	if pending, _ := spool.ListPending(); len(pending) != 0 {
		t.Fatalf("fatal entry stayed pending: %#v", pending)
	}
}

func TestSpoolDeadLettersAfterMaxAttempts(t *testing.T) {
	spool := testSpool(t)
	if _, err := spool.Enqueue(testRegistration("exec-a", PhaseTerminal, 0)); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{failures: 1 << 20}
	for i := 0; i < 3; i++ {
		pending, err := spool.ListPending()
		if err != nil || len(pending) == 0 {
			t.Fatalf("attempt %d: pending=%#v err=%v", i, pending, err)
		}
		entry := pending[0]
		entry.NextAttemptAt = time.Time{} // force due
		if err := rewriteEntry(spool, entry); err != nil {
			t.Fatal(err)
		}
		if _, err := spool.Flush(context.Background(), sender, 3); err != nil {
			t.Fatal(err)
		}
	}
	dead, _ := spool.ListDead()
	if len(dead) != 1 {
		t.Fatalf("entry not dead-lettered after max attempts: %#v", dead)
	}
	if dead[0].Attempts != 3 {
		t.Fatalf("attempts = %d", dead[0].Attempts)
	}
}

// rewriteEntry forces a pending entry's next attempt time for tests.
func rewriteEntry(spool *Spool, entry Entry) error {
	return writeFileAtomic(spool.pendingPath(entryKey(entry.ExecutionID, entry.Phase)), mustMarshal(entry))
}

func mustMarshal(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func TestSpoolOverflowDeadLettersOldest(t *testing.T) {
	spool := testSpool(t)
	spool.maxPending = 2
	for i := 0; i < 3; i++ {
		if _, err := spool.Enqueue(testRegistration(fmt.Sprintf("exec-%d", i), PhaseTerminal, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if pending, _ := spool.ListPending(); len(pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending))
	}
	dead, _ := spool.ListDead()
	if len(dead) != 1 || dead[0].ExecutionID != fixtureExecID("exec-0") || dead[0].LastError != "spool_full" {
		t.Fatalf("oldest not dead-lettered: %#v", dead)
	}
}

func TestSendErrorClassification(t *testing.T) {
	fatal := &SendError{Code: "auth_required", Fatal: true}
	if retryableSendError(fatal) {
		t.Fatal("auth errors must not retry")
	}
	if got := classifyDeadReason(fatal); got != ReasonAuthRequired {
		t.Fatalf("reason = %s", got)
	}
	notFound := &SendError{Code: "not_found", Fatal: true}
	if got := classifyDeadReason(notFound); got != ReasonCardUnresolved {
		t.Fatalf("reason = %s", got)
	}
	transient := &SendError{Detail: "timeout"}
	if !retryableSendError(transient) {
		t.Fatal("transient errors must retry")
	}
	if !retryableSendError(errors.New("plain")) {
		t.Fatal("unclassified errors must retry")
	}
}
