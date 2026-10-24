//go:build windows

package anxrun

import "os/exec"

// ANX run-link workers are unsupported on Windows until process-group/job
// cleanup is implemented. Fail closed rather than promise descendant cleanup.
func isolateANXProcess(cmd *exec.Cmd) error {
	return &SendError{Fatal: true, Reason: ReasonRejected, NumericCode: 125}
}
