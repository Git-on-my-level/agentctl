package main

import (
	"os"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
)

func retainNativeOwner(execution *model.Execution) {
	if execution.OwnerLaunch == nil {
		pid := os.Getpid()
		if execution.ProviderQueued() {
			execution.OwnerLaunch = queuedNativeOwnerLaunch(pid)
		} else {
			execution.OwnerLaunch = nativeOwnerLaunch(pid, processStart(pid))
		}
	}
}

func queuedNativeOwnerLaunch(pid int) *model.LaunchIdentity {
	return nativeOwnerLaunch(pid, queuedProcessStart(pid))
}

func nativeOwnerLaunch(pid int, observed processObservation) *model.LaunchIdentity {
	if observed.err != nil || !observed.present || !observed.startKnown {
		return nil
	}
	started := observed.started
	if started.IsZero() {
		started = time.Now().UTC()
	} // Linux identity uses kernel ticks and namespace.
	return &model.LaunchIdentity{PID: pid, StartedAt: started, StartTicks: observed.startTicks, PIDNamespaceIno: observed.nsInode, ProcfsPIDNamespaceIno: observed.nsInode}
}

const processStartTolerance = 5 * time.Second

// processIdentity is the launcher-recorded owner reconcile is allowed to prove.
type processIdentity struct {
	PID                   int
	StartedAt             time.Time
	StartTicks            int64
	PIDNamespaceIno       uint64
	ProcfsPIDNamespaceIno uint64
	exactStart            bool
}

// processProof is identity evidence for one recorded launch PID. Gone is proof
// the recorded process cannot be the live PID. Alive is proof it still is.
// Anything else is not proof, and reconcile must not change the execution.
type processProof struct {
	PID               int        `json:"pid,omitempty"`
	RecordedStartedAt time.Time  `json:"recorded_started_at,omitzero"`
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
	launch := execution.Launch
	queuedOwner := launch == nil && execution.ProviderQueued()
	if queuedOwner {
		// A queued row cannot have launched. After grant, the owner can
		// disappear with a live child whose launch has not been recorded.
		launch = execution.OwnerLaunch
	}
	if launch == nil {
		return processIdentity{}, false
	}
	if launch.PID <= 0 || launch.PID > model.MaxLaunchPID || launch.StartedAt.IsZero() {
		return processIdentity{}, false
	}
	return processIdentity{PID: launch.PID, StartedAt: launch.StartedAt, StartTicks: launch.StartTicks, PIDNamespaceIno: launch.PIDNamespaceIno, ProcfsPIDNamespaceIno: launch.ProcfsPIDNamespaceIno, exactStart: queuedOwner}, true
}
