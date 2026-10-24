//go:build !windows

package anxrun

import (
	"os/exec"
	"syscall"
)

func isolateANXProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return terminateANXProcessGroup(cmd)
	}
	return nil
}

func terminateANXProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}
