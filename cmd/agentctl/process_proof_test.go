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
		proof := inspectProcess(processIdentity{PID: pid, StartedAt: time.Now().Add(-time.Hour)})
		if proof.Gone || proof.Alive || proof.Proof != "start_unproven" {
			t.Fatalf("unreadable process table must not prove absence: %+v (%v)", proof, observed.err)
		}
		return
	}

	self := processIdentity{PID: pid, StartedAt: observed.started, StartTicks: observed.startTicks, PIDNamespaceIno: observed.nsInode}
	alive := inspectProcess(self)
	if !alive.Alive || alive.Gone || alive.Proof != "pid_alive" || !alive.Present {
		t.Fatalf("current process was not recognized: %+v", alive)
	}
	if observed.startTicks > 0 {
		reused := inspectProcess(processIdentity{PID: pid, StartedAt: observed.started, StartTicks: observed.startTicks - 1, PIDNamespaceIno: observed.nsInode})
		if !reused.Gone || reused.Alive || reused.Proof != "pid_reused" {
			t.Fatalf("later kernel start ticks were not pid reuse: %+v", reused)
		}
		earlier := inspectProcess(processIdentity{PID: pid, StartedAt: observed.started.Add(-24 * time.Hour), StartTicks: observed.startTicks + 1000, PIDNamespaceIno: observed.nsInode})
		if earlier.Gone || earlier.Alive || earlier.Proof != "start_unproven" {
			t.Fatalf("earlier start ticks were treated as proof: %+v", earlier)
		}
		noTicks := inspectProcess(processIdentity{PID: pid, StartedAt: observed.started.Add(-24 * time.Hour), PIDNamespaceIno: observed.nsInode})
		if noTicks.Gone || noTicks.Alive || noTicks.Proof == "pid_reused" || noTicks.Proof != "start_unproven" {
			t.Fatalf("missing launch ticks must not claim pid reuse: %+v", noTicks)
		}
	} else {
		reused := inspectProcess(processIdentity{PID: pid, StartedAt: observed.started.Add(-24 * time.Hour)})
		if !reused.Gone || reused.Alive || reused.Proof != "pid_reused" {
			t.Fatalf("later kernel start was not pid reuse: %+v", reused)
		}
		unproven := inspectProcess(processIdentity{PID: pid, StartedAt: observed.started.Add(time.Hour)})
		if unproven.Gone || unproven.Alive || unproven.Proof != "start_unproven" {
			t.Fatalf("earlier kernel start was treated as proof: %+v", unproven)
		}
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
	absent := inspectProcess(processIdentity{PID: gonePID, StartedAt: time.Now().Add(-time.Minute), StartTicks: 1, PIDNamespaceIno: observed.nsInode})
	if observed.nsInode != 0 {
		if absent.Alive || absent.Proof == "pid_reused" || (absent.Proof != "pid_absent" && absent.Proof != "unproven") {
			t.Fatalf("exited process was not absent or unproven: %+v start=%+v", absent, processStart(gonePID))
		}
		return
	}
	if !absent.Gone || absent.Proof != "pid_absent" {
		t.Fatalf("exited process was not absent: %+v start=%+v", absent, processStart(gonePID))
	}
}
