//go:build darwin || linux

package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
	"go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

func TestOpenLockInteroperatesWithLegacyBoltAndSnapshots(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	journal, err := Open(path, Options{LockTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	// Snapshot must not try to lock a second descriptor against its own DB.
	err = journal.db.View(func(tx *bbolt.Tx) error { _, err := tx.WriteTo(io.Discard); return err })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Millisecond}); !errors.Is(err, bbolt.ErrTimeout) {
		t.Fatalf("legacy writer bypassed new lock: %v", err)
	}
	journal.Close()
	legacy, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { legacy.Close() })
	if _, err := Open(path, Options{ReadOnly: true, LockTimeout: 10 * time.Millisecond}); !errors.Is(err, ErrBusy) {
		t.Fatalf("new reader bypassed legacy writer: %v", err)
	}
	legacy.Close()
	reader, err := Open(path, Options{ReadOnly: true, LockTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	other, err := bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
}

func TestOpenLockAcquiresDuringShortReleaseWindow(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "journal.db")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	options := &bbolt.Options{Timeout: 40 * time.Millisecond}
	configureOpenLock(options)
	go func() {
		f, err := options.OpenFile(path, os.O_RDWR, 0600)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	// This release falls inside one old 50ms poll interval. The caller's
	// timeout is shorter than that interval, but still covers the release.
	time.Sleep(10 * time.Millisecond)
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("missed release inside lock timeout: %v", err)
	}
}
