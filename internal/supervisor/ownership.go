package supervisor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// The lock inode is permanent. Unlinking it would let a second process lock a
// different inode while the first still owns the state directory.
func lockStateDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, "supervisor.lock")
	if err := ensureOwnerStateFile(path); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(info) {
		f.Close()
		return nil, fmt.Errorf("%w: invalid supervisor lock", ErrInsecurePermissions)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return f, nil
}

// UnixListener normally unlinks its name on Close, even if another listener
// now owns that name. Capture the created inode and disable that behavior.
type ownedListener struct {
	net.Listener
	path     string
	identity os.FileInfo
	once     sync.Once
	err      error
}

func (ln *ownedListener) Close() error {
	ln.once.Do(func() {
		ln.err = ln.Listener.Close()
		current, err := os.Lstat(ln.path)
		if err == nil && os.SameFile(current, ln.identity) {
			_ = os.Remove(ln.path)
		}
	})
	return ln.err
}
