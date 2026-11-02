package adapter

import "errors"

// ProcPIDNamespace verifies that numeric /proc paths use the caller's PID
// namespace. An inherited procfs mount can expose an ancestor's PIDs even
// though /proc/self/ns/pid describes the caller's own namespace.
func ProcPIDNamespace(inode func(string) (uint64, error)) (uint64, error) {
	self, err := inode("/proc/self/ns/pid")
	if err != nil {
		return 0, err
	}
	view, err := inode("/proc/1/ns/pid")
	if err != nil {
		return 0, err
	}
	if self == 0 || view != self {
		return 0, errors.New("procfs PID namespace is unproven")
	}
	return self, nil
}
