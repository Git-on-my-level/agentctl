//go:build linux

package main

import (
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

func processStart(pid int) processObservation {
	return linuxProcessStart(pid, os.ReadFile, selfPIDNamespaceInode, nil)
}

func queuedProcessStart(pid int) processObservation {
	return linuxProcessStart(pid, os.ReadFile, queuedPIDNamespaceInode, func() { time.Sleep(10 * time.Millisecond) })
}

func inspectProcessPlatform(id processIdentity) processProof {
	namespace := selfPIDNamespaceInode
	if id.exactStart {
		namespace = queuedPIDNamespaceInode
	}
	return linuxProof(id, os.ReadFile, namespace)
}

func queuedPIDNamespaceInode() (uint64, error) {
	return linuxOwnProcNamespace(os.Getpid(), os.ReadFile, procNSInode)
}

func selfPIDNamespaceInode() (uint64, error) {
	return adapter.ProcPIDNamespace(procNSInode)
}

func procNSInode(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Ino), nil
}
