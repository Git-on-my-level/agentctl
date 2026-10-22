// Package unixsocket validates filesystem Unix socket address lengths before
// callers attempt filesystem mutations or connections.
package unixsocket

import "fmt"

// PathLengthError reports an address that cannot fit in the platform's sun_path
// buffer. Length and Limit count bytes, excluding the required terminating NUL.
type PathLengthError struct {
	Path   string
	Length int
	Limit  int
}

func (e *PathLengthError) Error() string {
	return fmt.Sprintf("Unix socket path is %d bytes; limit is %d bytes (sun_path has %d bytes including the terminating NUL): %q; choose a shorter socket directory", e.Length, e.Limit, e.Limit+1, e.Path)
}

// ValidatePathLength is read-only and does not resolve symlinks. Path security
// and ownership checks remain the responsibility of the caller.
func ValidatePathLength(path string) error {
	if len(path) > PathLimit {
		return &PathLengthError{Path: path, Length: len(path), Limit: PathLimit}
	}
	return nil
}
