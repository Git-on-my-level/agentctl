//go:build darwin || linux

package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"golang.org/x/sys/unix"
)

// Waiter owns a unique sidecar lock, so concurrent probes never lock each other
// out. Only this owner removes its file; a crash leaves unlocked residue.
type Waiter struct{ file *os.File }

func (w *Waiter) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	file := w.file
	w.file = nil
	err := file.Close()
	removeErr := os.Remove(file.Name())
	if err != nil {
		return err
	}
	return removeErr
}

// RegisterWaiter records an exact-ID await using a process-held exclusive lock.
// Exit (including process death) releases it. Multiple awaits may coexist.
func RegisterWaiter(ctx context.Context, journalPath string, id ids.ExecutionID) (*Waiter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := waiterDirectory(journalPath, id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	if err := verifyOwnerDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := verifyOwnerDirectory(path); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(path, "await-")
	if err != nil {
		return nil, err
	}
	waiter := &Waiter{file: file}
	// A read-only query can briefly hold a shared probe between file creation
	// and registration. Retry within a bounded budget instead of failing await.
	lockCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return waiter, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = waiter.Close()
			return nil, err
		}
		select {
		case <-lockCtx.Done():
			_ = waiter.Close()
			return nil, ErrBusy
		case <-time.After(time.Millisecond):
		}
	}
}

func waiterDirectory(journalPath string, id ids.ExecutionID) (string, error) {
	if _, err := ids.ParseExecutionID(id.String()); err != nil {
		return "", err
	}
	path, err := filepath.Abs(journalPath + ".waiters/" + id.String())
	if err != nil {
		return "", err
	}
	return path, rejectSymlinks(path)
}

// hasLiveWaiter never creates files or changes journal state. Shared probes
// coexist; only a registered waiter's exclusive lock proves coverage. Unlocked
// files left by SIGKILL do not count. Bounds fail closed on excessive residue.
func hasLiveWaiter(ctx context.Context, journalPath string, id ids.ExecutionID) (bool, error) {
	path, err := waiterDirectory(journalPath, id)
	if err != nil {
		return false, err
	}
	dir, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer dir.Close()
	if err := verifyOwnerDirectory(path); err != nil {
		return false, err
	}
	names, err := dir.Readdirnames(129)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if len(names) > 128 {
		return false, ErrChildrenBounded
	}
	live := false
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		filePath := filepath.Join(path, name)
		if err := verifyOwnerFile(filePath); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			} // waiter just exited
			return false, err
		}
		file, err := os.OpenFile(filePath, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			live = true
		} else if err != nil {
			return false, err
		}
	}
	return live, nil
}
