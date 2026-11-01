//go:build windows

package nativecatalog

import "os/exec"

func isolateProbe(cmd *exec.Cmd) {}

func killProbe(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
