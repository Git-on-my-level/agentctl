package main

import (
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
)

const processStartTolerance = 5 * time.Second

// processIdentity is the launcher-recorded owner reconcile is allowed to prove.
type processIdentity struct {
	PID             int
	StartedAt       time.Time
	StartTicks      int64
	PIDNamespaceIno uint64
}

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
	startTicks int64
	nsInode    uint64
	err        error
}

func (a *app) proveProcess(id processIdentity) processProof {
	if a != nil && a.processProof != nil {
		proof := a.processProof(id)
		return stampProcessProof(proof, id)
	}
	return inspectProcess(id)
}

func inspectProcess(id processIdentity) processProof {
	return stampProcessProof(inspectProcessPlatform(id), id)
}

func stampProcessProof(proof processProof, id processIdentity) processProof {
	proof.PID = id.PID
	if !id.StartedAt.IsZero() {
		proof.RecordedStartedAt = id.StartedAt.UTC()
	}
	return proof
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func recordedLaunch(execution model.Execution) (processIdentity, bool) {
	if execution.Launch == nil {
		return processIdentity{}, false
	}
	launch := execution.Launch
	if launch.PID <= 0 || launch.PID > model.MaxLaunchPID || launch.StartedAt.IsZero() {
		return processIdentity{}, false
	}
	return processIdentity{PID: launch.PID, StartedAt: launch.StartedAt, StartTicks: launch.StartTicks, PIDNamespaceIno: launch.PIDNamespaceIno}, true
}
