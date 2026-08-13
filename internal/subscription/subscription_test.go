package subscription

import (
	"errors"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/callback"
)

func TestFilterAuthorityNormalizesDirectAliasAndRejectsUnknown(t *testing.T) {
	filter, err := (EventFilter{Authority: "direct"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if filter.Authority != "native" {
		t.Fatalf("direct authority normalized to %q", filter.Authority)
	}
	if _, err := (EventFilter{Authority: "typo"}).Normalize(); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("unknown authority error=%v", err)
	}
}

func testFilter() EventFilter {
	return EventFilter{ExecutionIDs: []string{"exec-b", "exec-a"}, Kinds: []string{"terminal", "attention"}}
}

func testSub(id string) Subscription {
	return Subscription{ID: id, Origin: "host-a", Filter: testFilter(), Destination: callback.Destination{Kind: callback.DestinationStdout}, State: StateActive}
}

func TestFilterDigestBindsAllFields(t *testing.T) {
	a, err := testFilter().Digest()
	if err != nil {
		t.Fatal(err)
	}
	b, err := (EventFilter{ExecutionIDs: []string{"exec-a", "exec-b"}, Kinds: []string{"attention", "terminal"}}).Digest()
	if err != nil || a != b {
		t.Fatalf("reordering changed digest: %s %s %v", a, b, err)
	}
	c, err := (EventFilter{ExecutionIDs: []string{"exec-a"}, Kinds: []string{"attention", "terminal"}}).Digest()
	if err != nil || c == a {
		t.Fatal("target change did not change digest")
	}
}

func TestCursorBindingAndExpiry(t *testing.T) {
	now := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	c, err := NewCursor("cursor-1", "host-a", "journal", 2, "event-2", "sha256:hash", testFilter(), now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(now, "host-a", "journal", testFilter()); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(now.Add(2*time.Minute), "host-a", "journal", testFilter()); err != ErrCursorExpired {
		t.Fatalf("expiry = %v", err)
	}
	if err := c.Validate(now, "host-b", "journal", testFilter()); err != ErrCursorMismatch {
		t.Fatalf("origin mismatch = %v", err)
	}
}

func TestRotationStopsOldOnlyAfterDurableReplacement(t *testing.T) {
	old := testSub("sub-old")
	newSub := testSub("sub-new")
	if _, _, _, err := Rotate(old, newSub, false); err != ErrNotDurable {
		t.Fatalf("non-durable rotation = %v", err)
	}
	oldAfter, newAfter, rotation, err := Rotate(old, newSub, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rotation.Committed || oldAfter.State != StateStopped || oldAfter.ReplacedBy != newAfter.ID || newAfter.Replaces != oldAfter.ID || newAfter.State != StateActive {
		t.Fatalf("bad rotation: old=%+v new=%+v rotation=%+v", oldAfter, newAfter, rotation)
	}
}
