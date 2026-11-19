package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"golang.org/x/sys/unix"
)

// CheckDispatchJournal requires one pathname for the journal's activation locks.
func CheckDispatchJournal(path string) error {
	var info unix.Stat_t
	if err := unix.Lstat(path, &info); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Nlink != 1 {
		return fmt.Errorf("%w: dispatch requires a regular journal with a single filesystem link", ErrPermission)
	}
	return nil
}

// LockDispatchActivation serializes replays of a bound dispatch in one journal.
// The caller can hold this lock across Multica I/O without holding the database
// open. Keep the empty sidecar so concurrent callers always lock the same inode.
func LockDispatchActivation(ctx context.Context, journalPath string, id ids.ExecutionID) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := ids.ParseExecutionID(id.String()); err != nil {
		return nil, err
	}
	dir := journalPath + ".dispatch"
	if err := rejectSymlinks(dir); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := verifyOwnerDirectory(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id.String()+".lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		file.Close()
		return nil, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0o077 != 0 || info.Uid != uint32(os.Geteuid()) {
		file.Close()
		return nil, ErrPermission
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			// A poll may win its select just as the deadline expires.
			if err := ctx.Err(); err != nil {
				file.Close()
				return nil, err
			}
			if err := CheckDispatchJournal(journalPath); err != nil {
				file.Close()
				return nil, err
			}
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
