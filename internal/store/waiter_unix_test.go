//go:build darwin || linux

package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

func TestWaiterProcessHelper(t *testing.T) {
	if os.Getenv("AGENTCTL_TEST_WAITER") != "1" {
		return
	}
	id, err := ids.ParseExecutionID(os.Getenv("AGENTCTL_TEST_WAITER_ID"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := RegisterWaiter(context.Background(), os.Getenv("AGENTCTL_TEST_WAITER_PATH"), id)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Println("ready")
	// Parent kills this helper to exercise kernel release without defers.
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

func TestChildrenLiveWaiterReleaseCrashAndState(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := sampleExecution(now)
	e.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "explicit"}
	e, _, err := j.CreateExecution(ctx, e, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	query := func(want int) {
		t.Helper()
		got, err := cleanupForPath(path).Children(ctx, ChildrenScope{Provider: "codex", NativeSessionID: hash('a')}, now)
		if err != nil || len(got) != want {
			t.Fatalf("children: %+v %v, want %d", got, err, want)
		}
	}
	query(1)
	first, err := RegisterWaiter(ctx, path, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := RegisterWaiter(ctx, path, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	query(0)
	first.Close()
	query(0)
	second.Close()
	query(1)

	childCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestWaiterProcessHelper$")
	cmd.Env = append(os.Environ(), "AGENTCTL_TEST_WAITER=1", "AGENTCTL_TEST_WAITER_ID="+e.ID.String(), "AGENTCTL_TEST_WAITER_PATH="+path)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("helper: %q %v", line, err)
	}
	query(0)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	query(1) // unlocked crash residue must not hide a child

	live, err := RegisterWaiter(ctx, path, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	for _, state := range []model.State{model.StateAttention, model.StateCompleted} {
		write, err := Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		e, err = write.GetExecution(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		e.State = state
		if state.Terminal() {
			e.TerminalAt, e.Liveness = &now, model.LivenessExited
		}
		_, err = write.UpdateExecution(ctx, e, e.Revision)
		write.Close()
		if err != nil {
			t.Fatal(err)
		}
		query(1) // attention and uncollected terminals remain actionable
	}
}

func TestWaiterProbesConcurrentReadOnlyAndBounded(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	if got, err := hasLiveWaiter(ctx, path, e.ID); err != nil || got {
		t.Fatalf("absent: %v %v", got, err)
	}
	if _, err := os.Stat(path + ".waiters"); !os.IsNotExist(err) {
		t.Fatalf("read created sidecars: %v", err)
	}
	w, err := RegisterWaiter(ctx, path, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	filePath := w.file.Name()
	w.Close()
	// An unlocked crash residue and simultaneous shared probes must never
	// prove coverage. Keep a shared probe open to make the distinction explicit.
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 30 {
				got, err := hasLiveWaiter(ctx, path, e.ID)
				if err != nil || got {
					t.Errorf("concurrent probe became waiter: %v %v", got, err)
					return
				}
			}
		}()
	}
	workers.Wait()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("probe mutated journal", err)
	}
	dir := filepath.Dir(filePath)
	for i := range 128 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("residue-%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := hasLiveWaiter(ctx, path, e.ID); !errors.Is(err, ErrChildrenBounded) || got {
		t.Fatalf("sidecar bound: %v %v", got, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if w, err := RegisterWaiter(cancelled, path, e.ID); !errors.Is(err, context.Canceled) || w != nil {
		t.Fatalf("cancelled registration: %v %v", w, err)
	}
}
