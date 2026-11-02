//go:build linux

package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

func hostBootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			seconds, err := strconv.ParseInt(fields[1], 10, 64)
			if err == nil && seconds > 0 {
				return time.Unix(seconds, 0).UTC()
			}
		}
	}
	return time.Time{}
}

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
