package main

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

func linuxProcessStart(pid int, read func(string) ([]byte, error), namespace func() (uint64, error), retry func()) processObservation {
	path := "/proc/" + strconv.Itoa(pid) + "/stat"
	data, err := read(path)
	for attempt := 0; retry != nil && errors.Is(err, os.ErrNotExist) && attempt < 4; attempt++ {
		retry()
		data, err = read(path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return processObservation{}
	}
	if err != nil {
		return processObservation{err: err}
	}
	ticks, err := adapter.ProcStartTicks(data)
	if err != nil {
		return processObservation{present: true, err: err}
	}
	ino, err := namespace()
	if err != nil {
		return processObservation{present: true, err: err}
	}
	return processObservation{present: true, startKnown: ticks > 0 && ino != 0, startTicks: ticks, nsInode: ino}
}

// Validate the launcher's numeric procfs view without reading PID 1's ns
// symlink, which ordinary users may lack permission to follow.
func linuxOwnProcNamespace(pid int, read func(string) ([]byte, error), inode func(string) (uint64, error)) (uint64, error) {
	status, err := read("/proc/self/status")
	if err != nil {
		return 0, err
	}
	if err := validateAdmissionProcView(string(status), pid); err != nil {
		return 0, err
	}
	return inode("/proc/self/ns/pid")
}

func validateAdmissionProcView(status string, pid int) error {
	for line := range strings.SplitSeq(status, "\n") {
		if value, found := strings.CutPrefix(line, "NSpid:"); found {
			ids := strings.Fields(value)
			// The leftmost value is relative to the procfs mount. Additional
			// values indicate an ancestor view, even when the numbers match.
			if len(ids) == 1 && ids[0] == strconv.Itoa(pid) {
				return nil
			}
			break
		}
	}
	return errors.New("procfs PID namespace does not match the launcher")
}

// linuxProof decides Linux process identity from an injected /proc reader.
// Absence is pid_absent only when the recorded pid namespace matches
// a validated procfs PID namespace, attested at launch, and a known-live pid (/proc/1 or this process's parent)
// is visible. Otherwise ENOENT is unproven: hidepid and a foreign pid
// namespace both hide live processes. A visible PID also requires a matching
// recorded namespace: a different namespace can alias a live unrelated PID.
// Start identity compares raw stat
// starttime ticks. Rows without ticks recorded at launch stay unproven
// instead of pid_reused, because boot-time plus ticks wall-clock math moves
// when NTP steps the clock.
func linuxProof(id processIdentity, read func(string) ([]byte, error), nsInode func() (uint64, error)) processProof {
	proof := processProof{PID: id.PID}
	if id.PID <= 0 {
		proof.Proof = "no_pid"
		return proof
	}
	if id.ProcfsPIDNamespaceIno == 0 || id.ProcfsPIDNamespaceIno != id.PIDNamespaceIno || !linuxNamespaceMatches(id.PIDNamespaceIno, nsInode) {
		proof.Proof = "unproven"
		return proof
	}
	data, err := read("/proc/" + strconv.Itoa(id.PID) + "/stat")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if linuxAbsenceProven(id.PIDNamespaceIno, read, nsInode) {
				proof.Proof = "pid_absent"
				proof.Gone = true
				return proof
			}
			proof.Proof = "unproven"
			return proof
		}
		proof.Proof = "start_unproven"
		return proof
	}
	ticks, err := adapter.ProcStartTicks(data)
	if err != nil {
		proof.Present = true
		proof.Proof = "start_unproven"
		return proof
	}
	proof.Present = true
	if id.StartTicks <= 0 {
		proof.Proof = "start_unproven"
		return proof
	}
	if ticks == id.StartTicks {
		proof.Proof = "pid_alive"
		proof.Alive = true
		return proof
	}
	if ticks > id.StartTicks || id.exactStart {
		proof.Proof = "pid_reused"
		proof.Gone = true
		return proof
	}
	proof.Proof = "start_unproven"
	return proof
}

func linuxAbsenceProven(recordedNS uint64, read func(string) ([]byte, error), nsInode func() (uint64, error)) bool {
	if !linuxNamespaceMatches(recordedNS, nsInode) {
		return false
	}
	if _, err := read("/proc/1/stat"); err == nil {
		return true
	}
	self, err := read("/proc/self/stat")
	if err != nil {
		return false
	}
	ppid, err := adapter.ProcParentPID(self)
	if err != nil {
		return false
	}
	_, err = read("/proc/" + strconv.Itoa(ppid) + "/stat")
	return err == nil
}

func linuxNamespaceMatches(recordedNS uint64, nsInode func() (uint64, error)) bool {
	if recordedNS == 0 || nsInode == nil {
		return false
	}
	current, err := nsInode()
	return err == nil && current != 0 && current == recordedNS
}
