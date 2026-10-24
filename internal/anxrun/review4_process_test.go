//go:build !windows

package anxrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReview4ANXExitTerminatesDescendants(t *testing.T) {
	for _, code := range []string{"0", "7"} {
		for _, pipes := range []string{"closed", "inherited"} {
			t.Run(code+"/"+pipes, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("AGENTCTL_ANX_EXIT_FIXTURE", root)
				path := filepath.Join(root, "anx")
				script := `#!/usr/bin/env python3
import os,pathlib,subprocess,sys,time
root=pathlib.Path(os.environ['AGENTCTL_ANX_EXIT_FIXTURE'])
if '--child' in sys.argv:
 deadline=time.monotonic()+10
 while not (root/'release').exists() and time.monotonic()<deadline:
  (root/'pulse').write_text(str(time.monotonic_ns()))
  time.sleep(.01)
else:
 kwargs={} if sys.argv[2]=='inherited' else {'stdout':subprocess.DEVNULL,'stderr':subprocess.DEVNULL}
 subprocess.Popen([sys.executable,__file__,'--child'],**kwargs)
 deadline=time.monotonic()+5
 while not (root/'pulse').exists() and time.monotonic()<deadline: time.sleep(.01)
 sys.exit(int(sys.argv[1]))
`
				if err := os.WriteFile(path, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = os.WriteFile(filepath.Join(root, "release"), nil, 0600)
					time.Sleep(100 * time.Millisecond)
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				started := time.Now()
				_, err := runAnxProcess(ctx, []string{path, code, pipes}, nil)
				if time.Since(started) > 2*time.Second {
					t.Fatal("exit cleanup exceeded pipe deadline")
				}
				if code == "0" && pipes == "closed" && err != nil {
					t.Fatal(err)
				}
				if code == "7" && err == nil {
					t.Fatal("error exit lost")
				}
				before, err := os.ReadFile(filepath.Join(root, "pulse"))
				if err != nil {
					t.Fatal("child never started")
				}
				time.Sleep(150 * time.Millisecond)
				after, _ := os.ReadFile(filepath.Join(root, "pulse"))
				if string(before) != string(after) {
					t.Error("descendant survived parent exit")
				}
				cancel() // Cancellation after Run must not be needed for cleanup.
			})
		}
	}
}
