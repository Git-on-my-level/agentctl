//go:build unix

package unixsocket

import "golang.org/x/sys/unix"

// PathLimit reserves one byte for the terminating NUL required by Go's
// filesystem Unix socket implementation (103 usable bytes on macOS).
const PathLimit = len(unix.RawSockaddrUnix{}.Path) - 1
