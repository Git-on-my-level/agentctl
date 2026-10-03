package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestInspectProcessProvesPidIdentity(t *testing.T) {
	pid := os.Getpid()
	observed := processStart(pid)
	if observed.err != nil || !observed.present || !observed.startKnown {
		proof := inspectProcess(pid, time.Now().Add(-time.Hour))
		if proof.Gone || proof.Alive || proof.Proof != "start_unproven" {
			t.Fatalf("unreadable process table must not prove absence: %+v (%v)", proof, observed.err)
		}
		return
	}

	alive := inspectProcess(pid, observed.started)
	if !alive.Alive || alive.Gone || alive.Proof != "pid_alive" || !alive.Present {
		t.Fatalf("current process was not recognized: %+v", alive)
	}
	reused := inspectProcess(pid, observed.started.Add(-24*time.Hour))
	if !reused.Gone || reused.Alive || reused.Proof != "pid_reused" {
		t.Fatalf("later kernel start was not pid reuse: %+v", reused)
	}
	unproven := inspectProcess(pid, observed.started.Add(time.Hour))
	if unproven.Gone || unproven.Alive || unproven.Proof != "start_unproven" {
		t.Fatalf("earlier kernel start was treated as proof: %+v", unproven)
	}

	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gonePID := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	absent := inspectProcess(gonePID, time.Now().Add(-time.Minute))
	if !absent.Gone || absent.Proof != "pid_absent" {
		t.Fatalf("exited process was not absent: %+v start=%+v", absent, processStart(gonePID))
	}
}
