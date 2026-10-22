//go:build !windows

package main

import (
	"os/exec"
	"os/signal"
	"syscall"
)

func prepareDetachedCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// A parent may disappear before reading the launch receipt. Broken receipt
// pipes must not terminate the independent native owner.
func ignoreDetachedPipeSignal() { signal.Ignore(syscall.SIGPIPE) }
