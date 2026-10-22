package unixsocket

import (
	"errors"
	"strings"
	"testing"
)

func TestPathLengthBoundaryCountsBytes(t *testing.T) {
	for _, path := range []string{strings.Repeat("x", PathLimit), strings.Repeat("é", PathLimit/2)} {
		if err := ValidatePathLength(path); err != nil {
			t.Fatalf("accepted boundary: %v", err)
		}
	}
	for _, path := range []string{strings.Repeat("x", PathLimit+1), strings.Repeat("é", PathLimit/2+1)} {
		err := ValidatePathLength(path)
		var length *PathLengthError
		if !errors.As(err, &length) || length.Length != len(path) || length.Limit != PathLimit || length.Path != path {
			t.Fatalf("length error: %#v", err)
		}
		if !strings.Contains(err.Error(), "sun_path") || !strings.Contains(err.Error(), "shorter socket directory") {
			t.Fatalf("missing remediation: %v", err)
		}
	}
}
