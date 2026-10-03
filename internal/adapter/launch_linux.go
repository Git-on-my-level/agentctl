//go:build linux

package adapter

import (
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func captureProcessLaunch(pid int, started time.Time) ProcessLaunch {
	launch := ProcessLaunch{PID: pid, StartedAt: started.UTC()}
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if ticks, err := ProcStartTicks(data); err == nil {
			launch.StartTicks = ticks
		}
	}
	if ino, err := pidNamespaceInode(); err == nil {
		launch.PIDNamespaceIno = ino
	}
	return launch
}

func pidNamespaceInode() (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil {
		return 0, err
	}
	return uint64(st.Ino), nil
}
