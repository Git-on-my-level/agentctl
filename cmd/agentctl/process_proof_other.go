//go:build !darwin && !linux

package main

import (
	"errors"
	"time"
)

func hostBootTime() time.Time { return time.Time{} }

func processStart(int) processObservation {
	return processObservation{err: errors.New("process start time is not available on this platform")}
}

func inspectProcessPlatform(id processIdentity) processProof {
	return processProof{Proof: "start_unproven"}
}
