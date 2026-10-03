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

func inspectProcessPlatform(id processIdentity) processProof {
	proof := processProof{}
	if id.PID <= 0 {
		proof.Proof = "no_pid"
		return proof
	}
	obs := processStart(id.PID)
	if obs.err != nil {
		proof.Proof = "start_unproven"
		return proof
	}
	if !obs.present {
		proof.Proof = "pid_absent"
		proof.Gone = true
		return proof
	}
	proof.Present = true
	if !obs.startKnown || id.StartedAt.IsZero() {
		proof.Proof = "start_unproven"
		return proof
	}
	started := obs.started.UTC()
	proof.ProcessStartedAt = &started
	recorded := id.StartedAt.UTC()
	if absDuration(started.Sub(recorded)) <= processStartTolerance {
		proof.Proof = "pid_alive"
		proof.Alive = true
		return proof
	}
	// A later kernel start time means this PID was reused after the recorded
	// process exited. An earlier start time can also be clock skew, so it is
	// not proof.
	if started.After(recorded.Add(processStartTolerance)) {
		proof.Proof = "pid_reused"
		proof.Gone = true
		return proof
	}
	proof.Proof = "start_unproven"
	return proof
}
