//go:build windows

package adapter

import "os/exec"

func prepareProcess(cmd *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd) error { return killProcess(cmd) }

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
