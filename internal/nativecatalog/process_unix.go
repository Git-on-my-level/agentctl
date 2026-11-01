//go:build !windows

package nativecatalog

import (
	"os/exec"
	"syscall"
)

// isolateProbe gives a probe its own process group, so a timeout also ends
// helpers that a wrapper script started.
func isolateProbe(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		killProbe(cmd)
		return nil
	}
}

func killProbe(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
