//go:build darwin || linux

package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

func TestLiveWaiterLocksAndConcurrentQueries(t *testing.T) {
	j, _, _ := openTestJournal(t)
	path := j.path
	j.Close()
	id, _ := ids.ParseExecutionID(durableTestID(ids.TypeExecution, 991))
	first, err := RegisterWaiter(path, id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RegisterWaiter(path, id)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, closeFirst := range []bool{false, true} {
		if closeFirst {
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				live, err := hasLiveWaiter(context.Background(), path, id)
				if err != nil || !live {
					t.Errorf("live=%v err=%v", live, err)
				}
			}()
		}
		wg.Wait()
	}
	second.Close()
	// Concurrent read probes must never count one another as collectors.
	for range 30 {
		var wg sync.WaitGroup
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				live, err := hasLiveWaiter(context.Background(), path, id)
				if err != nil || live {
					t.Errorf("stale live=%v err=%v", live, err)
				}
			}()
		}
		wg.Wait()
	}
}

func TestWaiterCrashHelper(t *testing.T) {
	path := os.Getenv("AGENTCTL_TEST_WAITER_PATH")
	if path == "" {
		return
	}
	id, _ := ids.ParseExecutionID(os.Getenv("AGENTCTL_TEST_WAITER_ID"))
	waiter, err := RegisterWaiter(path, id)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	fmt.Println("ready")
	time.Sleep(time.Minute)
}

func TestLiveWaiterCrashReleasesEvidence(t *testing.T) {
	j, _, _ := openTestJournal(t)
	path := j.path
	j.Close()
	id, _ := ids.ParseExecutionID(durableTestID(ids.TypeExecution, 992))
	command := exec.Command(os.Args[0], "-test.run=^TestWaiterCrashHelper$")
	command.Env = append(os.Environ(), "AGENTCTL_TEST_WAITER_PATH="+path, "AGENTCTL_TEST_WAITER_ID="+id.String())
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		live, err := hasLiveWaiter(context.Background(), path, id)
		if err != nil {
			t.Fatal(err)
		}
		if live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not acquire waiter")
		}
		time.Sleep(10 * time.Millisecond)
	}
	command.Process.Kill()
	command.Wait()
	live, err := hasLiveWaiter(context.Background(), path, id)
	if err != nil || live {
		t.Fatalf("crashed waiter live=%v err=%v", live, err)
	}
	entries, _ := filepath.Glob(filepath.Join(path+".waiters", id.String(), "*"))
	if len(entries) == 0 {
		t.Fatal("test did not leave an inert crash marker")
	}
}
