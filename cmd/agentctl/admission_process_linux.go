//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"golang.org/x/sys/unix"
)

// Admission verifies its own numeric procfs view without inspecting PID 1.
// The execution-repair proof uses a separate, stricter attestation contract.
var admissionLinuxScope = sync.OnceValues(func() (string, error) {
	ino, err := queuedPIDNamespaceInode()
	if err != nil {
		return "", err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	bootID := strings.TrimSpace(string(boot))
	if bootID == "" || ino == 0 {
		return "", errors.New("Linux process identity scope is unavailable")
	}
	return fmt.Sprintf("%s/%d", bootID, ino), nil
})

func admissionProcessStart(pid int) (string, error) {
	scope, err := admissionLinuxScope()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
			return "", nil
		}
		return "", err
	}
	ticks, err := adapter.ProcStartTicks(data)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%d", scope, ticks), nil
}
