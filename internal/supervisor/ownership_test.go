package supervisor

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStateOwnershipAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("AGENTCTL_TEST_LOCK_DIR"); dir != "" {
		lock, err := lockStateDir(dir)
		if errors.Is(err, ErrAlreadyRunning) {
			os.Exit(23)
		}
		if err != nil {
			os.Exit(24)
		}
		lock.Close()
		os.Exit(0)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	dir, _ = filepath.EvalSymlinks(dir)
	first, _ := New(Config{StateDir: dir}, Dependencies{})
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer first.Shutdown(context.Background())
	second, _ := New(Config{StateDir: dir, SocketPath: filepath.Join(dir, "another.sock")}, Dependencies{})
	if err := second.Start(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second start: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStateOwnershipAcrossProcesses$")
	cmd.Env = append(os.Environ(), "AGENTCTL_TEST_LOCK_DIR="+dir)
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("child contention: %v", err)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(os.Args[0], "-test.run=^TestStateOwnershipAcrossProcesses$")
	cmd.Env = append(os.Environ(), "AGENTCTL_TEST_LOCK_DIR="+dir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("released lock: %v", err)
	}
}

func TestSocketOwnership(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	dir, _ = filepath.EvalSymlinks(dir)
	path := filepath.Join(dir, "s.sock")
	old, err := listenOwnerSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := listenOwnerSocket(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("live socket: %v", err)
	}
	os.Remove(path)
	replacement, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	info, _ := os.Lstat(path)
	old.Close()
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		t.Fatal("old listener removed replacement")
	}
	replacement.(*net.UnixListener).SetUnlinkOnClose(false)
	replacement.Close()
	os.Chmod(path, 0600)
	recovered, err := listenOwnerSocket(path)
	if err != nil {
		t.Fatalf("stale recovery: %v", err)
	}
	recovered.Close()
}
