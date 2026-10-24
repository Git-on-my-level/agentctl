//go:build !windows

package anxrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReview3ANXCancellationReleasesFlushLockWithInheritedPipes(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "cancel"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) { review3InheritedPipe(t, timeout) })
	}
}

func review3InheritedPipe(t *testing.T, timeout bool) {
	root := t.TempDir()
	t.Setenv("AGENTCTL_ANX_PIPE_FIXTURE", root)
	path := filepath.Join(root, "anx")
	script := `#!/usr/bin/env python3
import os,pathlib,subprocess,sys,time
root=pathlib.Path(os.environ['AGENTCTL_ANX_PIPE_FIXTURE'])
if '--child' in sys.argv:
 deadline=time.monotonic()+15
 while not (root/'release').exists() and time.monotonic()<deadline: time.sleep(.01)
 (root/'done').touch()
else:
 subprocess.Popen([sys.executable,__file__,'--child'],start_new_session=True)
 (root/'ready').touch()
 time.sleep(15)
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(root, "release"), nil, 0600)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(root, "done")); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("fixture descendant did not exit")
	})
	s := testSpool(t)
	if _, err := s.Enqueue(testRegistration("exec-a", PhaseTerminal, 1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if timeout {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	}
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, err := s.Flush(ctx, senderFunc(func(ctx context.Context, _ Registration) error {
			_, err := runAnxProcess(ctx, []string{path}, nil)
			return err
		}), 0)
		first <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("anx fixture not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Now()
	if !timeout {
		cancel()
	}
	budget := time.Second
	if timeout {
		budget = 3 * time.Second
	}
	nextCtx, stop := context.WithTimeout(context.Background(), budget)
	defer stop()
	result, err := s.Flush(nextCtx, &recordingSender{}, 0)
	if err != nil {
		_ = os.WriteFile(filepath.Join(root, "release"), nil, 0600)
		<-first
		t.Fatalf("canceled ANX retained flush lock past cleanup deadline: %v", err)
	}
	expected := context.Canceled
	if timeout {
		expected = context.DeadlineExceeded
	}
	if err := <-first; err != expected {
		t.Fatalf("cancel result: %v", err)
	}
	if result.Delivered+result.SkippedNotDue != 1 || time.Since(started) > budget {
		t.Fatal("cleanup/next flush was not bounded")
	}
}

func TestReview3ANXCancelsEntireProcessGroup(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENTCTL_ANX_GROUP_FIXTURE", root)
	path := filepath.Join(root, "anx")
	script := `#!/usr/bin/env python3
import os,pathlib,subprocess,sys,time
root=pathlib.Path(os.environ['AGENTCTL_ANX_GROUP_FIXTURE'])
if '--child' in sys.argv:
 deadline=time.monotonic()+15
 while not (root/'release').exists() and time.monotonic()<deadline:
  (root/'pulse').write_text(str(time.monotonic()))
  time.sleep(.02)
 (root/'done').touch()
else:
 subprocess.Popen([sys.executable,__file__,'--child'])
 time.sleep(15)
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
	done := make(chan error, 1)
	go func() { _, err := runAnxProcess(ctx, []string{path}, nil); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "pulse")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("group fixture not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = os.WriteFile(filepath.Join(root, "release"), nil, 0600)
		<-done
		t.Fatal("cancellation did not close owned process group")
	}
	before, _ := os.ReadFile(filepath.Join(root, "pulse"))
	time.Sleep(150 * time.Millisecond)
	after, _ := os.ReadFile(filepath.Join(root, "pulse"))
	if string(before) != string(after) {
		t.Fatal("descendant survived group cancellation")
	}
}
