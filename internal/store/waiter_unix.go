//go:build darwin || linux

package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"golang.org/x/sys/unix"
)

// Waiter is process-local evidence, independent of the journal writer lock.
// Normal exits unlink their own inode; crashes leave an unlocked, inert file.
type Waiter struct {
	file *os.File
	path string
}

func (w *Waiter) Close() error {
	if w == nil {
		return nil
	}
	_ = os.Remove(w.path)
	return w.file.Close()
}

// RegisterWaiter publishes an exclusively locked, unique inode only after its
// lock is held. Descriptors are close-on-exec. SIGKILL/exit releases the lock.
func RegisterWaiter(path string, id ids.ExecutionID) (*Waiter, error) {
	if _, err := ids.ParseExecutionID(id.String()); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(filepath.Join(path+".waiters", id.String()))
	if err != nil {
		return nil, err
	}
	if err := rejectSymlinks(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrPermission
	}
	file, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return nil, err
	}
	pending := file.Name()
	fail := func(err error) (*Waiter, error) { file.Close(); os.Remove(pending); return nil, err }
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(err)
	}
	published := filepath.Join(dir, strings.TrimPrefix(filepath.Base(pending), ".pending-"))
	if err := os.Rename(pending, published); err != nil {
		return fail(err)
	}
	return &Waiter{file: file, path: published}, nil
}

// Queries never create files or take a journal write lock. Shared probes cannot
// mistake another query for a waiter: only an exclusive collector lock blocks.
func hasLiveWaiter(ctx context.Context, path string, id ids.ExecutionID) (bool, error) {
	dirPath, err := filepath.Abs(filepath.Join(path+".waiters", id.String()))
	if err != nil {
		return false, err
	}
	if err := rejectSymlinks(dirPath); err != nil {
		return false, err
	}
	dir, err := os.Open(dirPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(129)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if len(entries) > 128 {
		return false, ErrChildrenBounded
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		filePath := filepath.Join(dir.Name(), entry.Name())
		fd, err := unix.Open(filePath, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		file := os.NewFile(uintptr(fd), filePath)
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			file.Close()
			if statErr != nil {
				return false, statErr
			}
			return false, ErrPermission
		}
		err = unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB)
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	return false, nil
}
