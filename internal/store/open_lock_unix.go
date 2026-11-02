//go:build darwin || linux

package store

import (
	"errors"
	"math/rand/v2"
	"os"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

// bbolt polls flock every 50ms and treats Timeout<=50ms as one attempt. Scoped
// CLI opens can repeatedly miss short release windows. Acquire the exact same
// kernel lock on the exact descriptor bbolt will use, at a jittered 1-4ms pace.
// Its subsequent flock on that already-locked descriptor is idempotent. Old
// binaries use the same lock, so no second lock domain or format is introduced.
func configureOpenLock(options *bbolt.Options) {
	var first atomic.Bool
	options.OpenFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
		initial := first.CompareAndSwap(false, true)
		if initial && options.ReadOnly {
			flags &^= os.O_CREATE
		}
		file, err := os.OpenFile(path, flags, mode)
		if err != nil {
			return nil, err
		}
		// bbolt also uses OpenFile for Tx.WriteTo/CopyFile. Those are ordinary
		// file opens under its existing transaction, not new database owners.
		if !initial {
			return file, nil
		}
		flag := unix.LOCK_EX | unix.LOCK_NB
		if options.ReadOnly {
			flag = unix.LOCK_SH | unix.LOCK_NB
		}
		deadline := time.Now().Add(options.Timeout)
		for {
			err = unix.Flock(int(file.Fd()), flag)
			if err == nil {
				return file, nil
			}
			if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
				file.Close()
				return nil, err
			}
			if options.Timeout != 0 && !time.Now().Before(deadline) {
				file.Close()
				return nil, bbolt.ErrTimeout
			}
			delay := time.Duration(1+rand.IntN(4)) * time.Millisecond
			if options.Timeout != 0 {
				delay = min(delay, time.Until(deadline))
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}
	}
}
