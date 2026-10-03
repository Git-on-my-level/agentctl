package main

import (
	"errors"
	"os"
	"strconv"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

// linuxProof decides Linux process identity from an injected /proc reader.
// Absence is pid_absent only when the recorded pid namespace matches
// /proc/self/ns/pid and a known-live pid (/proc/1 or this process's parent)
// is visible. Otherwise ENOENT is unproven: hidepid and a foreign pid
// namespace both hide live processes. Start identity compares raw stat
// starttime ticks. Rows without ticks recorded at launch stay unproven
// instead of pid_reused, because boot-time plus ticks wall-clock math moves
// when NTP steps the clock.
func linuxProof(id processIdentity, read func(string) ([]byte, error), nsInode func() (uint64, error)) processProof {
	proof := processProof{PID: id.PID}
	if id.PID <= 0 {
		proof.Proof = "no_pid"
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
	if ticks > id.StartTicks {
		proof.Proof = "pid_reused"
		proof.Gone = true
		return proof
	}
	proof.Proof = "start_unproven"
	return proof
}

func linuxAbsenceProven(recordedNS uint64, read func(string) ([]byte, error), nsInode func() (uint64, error)) bool {
	if recordedNS == 0 || nsInode == nil {
		return false
	}
	current, err := nsInode()
	if err != nil || current == 0 || current != recordedNS {
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
