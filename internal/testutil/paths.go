// Package testutil provides filesystem fixtures without changing the caller's
// TMPDIR or the product's symlink policy.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TempDir preserves the configured temporary root and resolves system aliases
// before a fixture is passed to APIs that deliberately reject symlinks.
func TempDir(t testing.TB) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// SocketDir allocates an owner-only directory with a short name independent of
// TMPDIR and the test name. Unix socket addresses have a small byte limit.
func SocketDir(t testing.TB) string {
	t.Helper()
	root := "/tmp"
	if runtime.GOOS == "windows" {
		root = os.TempDir()
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.MkdirTemp(root, "ac-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})
	return path
}
