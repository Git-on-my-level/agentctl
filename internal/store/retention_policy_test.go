package store

import (
	"context"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestRetentionPolicyNewAndUpgradedJournals(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	at, err := j.RetentionEnforceAt(ctx)
	if err != nil || !at.Equal(now) {
		t.Fatalf("new journal=%s err=%v", at, err)
	}
	// Simulate a journal written by the previous version, including all existing
	// acknowledgement features but no retention policy metadata.
	if err := j.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bMetadata).Delete(keyRetentionEnforceAt) }); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	read, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	missing, err := read.RetentionEnforceAt(ctx)
	read.Close()
	if err != nil || !missing.IsZero() {
		t.Fatalf("read initialized policy=%s %v", missing, err)
	}
	upgradeAt := now.Add(time.Hour)
	write, err := Open(path, Options{Clock: func() time.Time { return upgradeAt }})
	if err != nil {
		t.Fatal(err)
	}
	at, err = write.RetentionEnforceAt(ctx)
	write.Close()
	if err != nil || !at.Equal(upgradeAt.Add(RetentionPreviewWindow)) {
		t.Fatalf("upgrade=%s %v", at, err)
	}
	restarted, err := Open(path, Options{Clock: func() time.Time { return upgradeAt.Add(3 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	again, err := restarted.RetentionEnforceAt(ctx)
	if err != nil || !again.Equal(at) {
		t.Fatalf("restart reset window=%s want=%s err=%v", again, at, err)
	}
}
