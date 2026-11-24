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
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "", err
	}
	if err := validateAdmissionProcView(string(status), os.Getpid()); err != nil {
		return "", err
	}
	var ns unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &ns); err != nil {
		return "", err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	bootID := strings.TrimSpace(string(boot))
	if bootID == "" || ns.Ino == 0 {
		return "", errors.New("Linux process identity scope is unavailable")
	}
	return fmt.Sprintf("%s/%d", bootID, ns.Ino), nil
})

func validateAdmissionProcView(status string, pid int) error {
	for line := range strings.SplitSeq(status, "\n") {
		if value, found := strings.CutPrefix(line, "NSpid:"); found {
			ids := strings.Fields(value)
			// The leftmost value is relative to the procfs mount. Additional
			// values mean a nested view even if the numbers happen to match.
			if len(ids) == 1 && ids[0] == strconv.Itoa(pid) {
				return nil
			}
			break
		}
	}
	return errors.New("procfs PID namespace does not match the launcher")
}

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
