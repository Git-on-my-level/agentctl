//go:build darwin

package main

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

func processStart(pid int) processObservation {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// x/sys returns EIO when kern.proc.pid succeeds with an empty result.
		// That empty result is how Darwin reports a PID that is not running.
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EIO) {
			return processObservation{}
		}
		return processObservation{err: err}
	}
	usec := info.Proc.P_starttime.Usec
	started := time.Unix(info.Proc.P_starttime.Sec, int64(usec)*int64(time.Microsecond)).UTC()
	return processObservation{present: true, started: started, startKnown: true}
}
