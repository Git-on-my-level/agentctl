package main

import (
	"time"
)

const processStartTolerance = 5 * time.Second

// processProof is identity evidence for one recorded launch PID. Gone is proof
// the recorded process cannot be the live PID. Alive is proof it still is.
// Anything else is not proof, and reconcile must not change the execution.
type processProof struct {
	PID               int        `json:"pid"`
	RecordedStartedAt time.Time  `json:"recorded_started_at,omitempty"`
	ProcessStartedAt  *time.Time `json:"process_started_at,omitempty"`
	Present           bool       `json:"process_present"`
	Proof             string     `json:"proof"`
	Gone              bool       `json:"-"`
	Alive             bool       `json:"-"`
}

type processObservation struct {
	present    bool
	started    time.Time
	startKnown bool
	err        error
}

func (a *app) proveProcess(pid int, recorded time.Time) processProof {
	if a != nil && a.processProof != nil {
		proof := a.processProof(pid, recorded)
		proof.PID = pid
		if !recorded.IsZero() {
			proof.RecordedStartedAt = recorded.UTC()
		}
		return proof
	}
	return inspectProcess(pid, recorded)
}

func inspectProcess(pid int, recorded time.Time) processProof {
	proof := processProof{PID: pid}
	if !recorded.IsZero() {
		proof.RecordedStartedAt = recorded.UTC()
	}
	if pid <= 0 {
		proof.Proof = "no_pid"
		return proof
	}
	obs := processStart(pid)
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
	if !obs.startKnown || recorded.IsZero() {
		proof.Proof = "start_unproven"
		return proof
	}
	started := obs.started.UTC()
	proof.ProcessStartedAt = &started
	if absDuration(started.Sub(recorded)) <= processStartTolerance {
		proof.Proof = "pid_alive"
		proof.Alive = true
		return proof
	}
	// A later start time means this PID was reused after the recorded process
	// exited. An earlier start time can also be clock skew, so it is not proof.
	if started.After(recorded.Add(processStartTolerance)) {
		proof.Proof = "pid_reused"
		proof.Gone = true
		return proof
	}
	proof.Proof = "start_unproven"
	return proof
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
