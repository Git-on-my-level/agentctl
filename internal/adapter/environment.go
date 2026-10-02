package adapter

import (
	"runtime"
	"strings"
)

const CallerHarnessEnv = "AGENTCTL_CALLER_HARNESS"

// WithoutCallerDeclaration removes an invocation's caller declaration from
// inherited child environments. A native child is a new caller; it must declare
// itself before coordinating further work. Explicit LaunchRequest.Env entries
// are applied after this filter, so a caller can set a child-specific value.
func WithoutCallerDeclaration(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != CallerHarnessEnv && !(runtime.GOOS == "windows" && strings.EqualFold(key, CallerHarnessEnv)) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
