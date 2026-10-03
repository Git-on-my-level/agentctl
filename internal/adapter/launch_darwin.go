//go:build darwin

package adapter

import "time"

func captureProcessLaunch(pid int, started time.Time) ProcessLaunch {
	return ProcessLaunch{PID: pid, StartedAt: started.UTC()}
}
