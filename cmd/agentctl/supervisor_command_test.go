package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

func TestLongRunningSupervisorReleasesJournalBetweenCycles(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "state", "journal.db")
	stateDir := filepath.Join(root, "d")
	// macOS Unix socket paths are short; keep this integration test's socket
	// path below that platform limit even though t.TempDir is nested.
	socketRoot, err := os.MkdirTemp(os.TempDir(), "as-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	socketPath := filepath.Join(socketRoot, "s")

	// Seed one durable execution so subscribe create is a real journal
	// mutation, not merely a competing open.
	seedOut, seedErr := &bytes.Buffer{}, &bytes.Buffer{}
	seed := testApp(seedOut, seedErr)
	if code := seed.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", `{"type":"result","status":"completed"}`}); code != 0 {
		t.Fatalf("seed run exit=%d stdout=%s stderr=%s", code, seedOut.String(), seedErr.String())
	}
	var seedDoc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(seedOut.Bytes(), &seedDoc); err != nil {
		t.Fatal(err)
	}
	if seedDoc.Result.ID.IsZero() {
		t.Fatal("seed execution ID is empty")
	}

	serviceOut, serviceErr := &bytes.Buffer{}, &bytes.Buffer{}
	serviceApp := testApp(serviceOut, serviceErr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serviceDone := make(chan int, 1)
	go func() {
		serviceDone <- serviceApp.run(ctx, []string{"--output", "json", "--journal", journalPath, "supervisor", "run", "--interval", "20ms", "--state-dir", stateDir, "--socket", socketPath})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if response, err := supervisor.Request(context.Background(), socketPath, "status"); err == nil && response.OK && response.Status != nil && response.Status.Running {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-serviceDone
			t.Fatalf("supervisor did not become ready; stdout=%s stderr=%s", serviceOut.String(), serviceErr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	mutationOut, mutationErr := &bytes.Buffer{}, &bytes.Buffer{}
	mutationApp := testApp(mutationOut, mutationErr)
	mutationDone := make(chan int, 1)
	go func() {
		mutationDone <- mutationApp.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "subscribe", "create", "--execution", seedDoc.Result.ID.String(), "--kind", "terminal", "--destination", "file", "--target", filepath.Join(root, "callback.ndjson")})
	}()
	select {
	case code := <-mutationDone:
		if code != 0 {
			t.Fatalf("concurrent journal mutation exit=%d stdout=%s stderr=%s", code, mutationOut.String(), mutationErr.String())
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-serviceDone
		t.Fatalf("concurrent journal mutation remained blocked; supervisor likely retained bbolt lock; stderr=%s", mutationErr.String())
	}

	cancel()
	select {
	case code := <-serviceDone:
		if code != 0 {
			t.Fatalf("supervisor exit=%d stdout=%s stderr=%s", code, serviceOut.String(), serviceErr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not shut down")
	}
}
