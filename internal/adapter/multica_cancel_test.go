//go:build !windows

package adapter

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A descendant can inherit stdout while outliving the canceled CLI's process
// group. Cancellation must not wait for that descendant to close the pipe.
func TestMulticaCancelledReadBoundsInheritedPipeDrain(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	root := t.TempDir()
	t.Setenv("AGENTCTL_CANCEL_PIPE_FIXTURE", root)
	path := filepath.Join(root, "multica")
	script := `#!/usr/bin/env python3
import os,pathlib,subprocess,sys,time
root=pathlib.Path(os.environ['AGENTCTL_CANCEL_PIPE_FIXTURE'])
if '--pipe-child' in sys.argv:
 deadline=time.monotonic()+20
 while not (root/'release').exists() and time.monotonic()<deadline: time.sleep(.01)
 (root/'child-done').touch()
else:
 subprocess.Popen([sys.executable,__file__,'--pipe-child'],start_new_session=True)
 (root/'ready').touch()
 time.sleep(20)
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(root, "release"), nil, 0600)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(root, "child-done")); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("fixture descendant did not finish")
	})
	m := &multicaAdapter{config: MulticaConfig{Binary: path, Profile: "fixture", Workspace: "fixture"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.readMulticaJSON(ctx, "issue get", "fixture"); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Now()
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancellation waited for inherited pipe: %s", elapsed)
	}
}
