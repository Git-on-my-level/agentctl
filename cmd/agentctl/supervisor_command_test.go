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

func TestDefaultSupervisorSocketFollowsStateContract(t *testing.T) {
	root := t.TempDir()
	a := &app{getenv: func(key string) string {
		switch key {
		case "AGENTCTL_STATE_HOME":
			return filepath.Join(root, "explicit")
		case "XDG_STATE_HOME":
			return filepath.Join(root, "xdg")
		default:
			return ""
		}
	}}
	got, problem := a.defaultSupervisorSocket()
	if problem != nil {
		t.Fatal(problem)
	}
	want := filepath.Join(root, "explicit", "supervisor.sock")
	if got != want {
		t.Fatalf("socket=%q want %q", got, want)
	}
	a.getenv = func(key string) string {
		if key == "XDG_STATE_HOME" {
			return filepath.Join(root, "xdg")
		}
		return ""
	}
	got, problem = a.defaultSupervisorSocket()
	if problem != nil {
		t.Fatal(problem)
	}
	want = filepath.Join(root, "xdg", "agentctl", "supervisor.sock")
	if got != want {
		t.Fatalf("socket=%q want %q", got, want)
	}
}

func TestLinuxSupervisorPlanUsesXDGConfigHome(t *testing.T) {
	root := t.TempDir()
	xdg := filepath.Join(root, "xdg-config")
	t.Setenv("HOME", filepath.Join(root, "home"))
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	a := testApp(stdout, stderr)
	a.getenv = func(key string) string {
		if key == "XDG_CONFIG_HOME" {
			return xdg
		}
		return ""
	}
	executable := filepath.Join(root, "agentctl")
	state := filepath.Join(root, "state")
	if code := a.run(context.Background(), []string{"--output", "json", "supervisor", "plan", "--platform", "linux", "--executable", executable, "--state-dir", state}); code != 0 {
		t.Fatalf("plan exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var doc struct {
		Result struct {
			Path string `json:"Path"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "systemd", "user", "io.agentctl.supervisor.service")
	if doc.Result.Path != want {
		t.Fatalf("path=%q want=%q", doc.Result.Path, want)
	}

	stdout.Reset()
	stderr.Reset()
	a.getenv = func(key string) string {
		if key == "XDG_CONFIG_HOME" {
			return "relative"
		}
		return ""
	}
	if code := a.run(context.Background(), []string{"--output", "json", "supervisor", "plan", "--platform", "linux", "--executable", executable, "--state-dir", state}); code != 2 {
		t.Fatalf("relative XDG_CONFIG_HOME exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

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
