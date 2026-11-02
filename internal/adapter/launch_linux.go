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
	ino, err := pidNamespaceInode()
	if err != nil {
		return launch
	}
	launch.PIDNamespaceIno = ino
	launch.ProcfsPIDNamespaceIno = ino
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if ticks, err := ProcStartTicks(data); err == nil {
			launch.StartTicks = ticks
		}
	}
	return launch
}

func pidNamespaceInode() (uint64, error) {
	return ProcPIDNamespace(func(path string) (uint64, error) {
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			return 0, err
		}
		return uint64(st.Ino), nil
	})
}
