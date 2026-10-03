//go:build linux

package main

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

func processStart(pid int) processObservation {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return processObservation{}
		}
		return processObservation{err: err}
	}
	ticks, err := adapter.ProcStartTicks(data)
	if err != nil {
		return processObservation{present: true, err: err}
	}
	obs := processObservation{present: true, startKnown: true, startTicks: ticks}
	if ino, err := selfPIDNamespaceInode(); err == nil {
		obs.nsInode = ino
	}
	return obs
}

func inspectProcessPlatform(id processIdentity) processProof {
	return linuxProof(id, os.ReadFile, selfPIDNamespaceInode)
}

func selfPIDNamespaceInode() (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil {
		return 0, err
	}
	return uint64(st.Ino), nil
}
