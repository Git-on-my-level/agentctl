package adapter

import (
	"runtime"
	"strings"
)

const CallerHarnessEnv = "AGENTCTL_CALLER_HARNESS"
const CallerProviderEnv = "AGENTCTL_CALLER_PROVIDER"
const CallerSessionEnv = "AGENTCTL_CALLER_NATIVE_SESSION_ID"

// mergeEnvironment replaces inherited entries with explicit launch values,
// including empty values. Repeated overrides use the last value.
func mergeEnvironment(env, overrides []string) []string {
	values := make(map[string]string, len(overrides))
	for _, entry := range overrides {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		} else {
			// Preserve exec.Cmd's handling of entries without an equals sign.
			env = append(env, entry)
		}
	}
	return setEnvironment(env, values)
}

// WithoutCallerDeclaration removes an invocation's caller declaration from
// inherited child environments. A native child is a new caller; it must declare
// itself before coordinating further work. Explicit LaunchRequest.Env entries
// are applied after this filter, so a caller can set a child-specific value.
func WithoutCallerDeclaration(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		declared := false
		for _, name := range []string{CallerHarnessEnv, CallerProviderEnv, CallerSessionEnv} {
			if key == name || (runtime.GOOS == "windows" && strings.EqualFold(key, name)) {
				declared = true
			}
		}
		if !declared {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
