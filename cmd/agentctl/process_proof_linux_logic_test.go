package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestLinuxENOENTWithoutNamespaceProofIsUnproven(t *testing.T) {
	read := func(name string) ([]byte, error) {
		if name == "/proc/1/stat" {
			return procStatLine(1, 0, 10), nil
		}
		return nil, os.ErrNotExist
	}
	sameNS := func() (uint64, error) { return 7, nil }
	otherNS := func() (uint64, error) { return 8, nil }
	id := processIdentity{PID: 42, StartedAt: time.Unix(1, 0).UTC(), StartTicks: 100, PIDNamespaceIno: 7}

	mismatch := linuxProof(id, read, otherNS)
	if mismatch.Gone || mismatch.Alive || mismatch.Proof != "unproven" {
		t.Fatalf("namespace mismatch proved absence: %+v", mismatch)
	}
	missingRecord := id
	missingRecord.PIDNamespaceIno = 0
	unrecorded := linuxProof(missingRecord, read, sameNS)
	if unrecorded.Gone || unrecorded.Proof != "unproven" {
		t.Fatalf("missing launch namespace proved absence: %+v", unrecorded)
	}
	hidden := linuxProof(id, func(string) ([]byte, error) { return nil, os.ErrNotExist }, sameNS)
	if hidden.Gone || hidden.Proof != "unproven" {
		t.Fatalf("ENOENT without a visible known pid proved absence: %+v", hidden)
	}

	absent := linuxProof(id, read, sameNS)
	if !absent.Gone || absent.Alive || absent.Proof != "pid_absent" {
		t.Fatalf("visible /proc/1 and matching namespace did not prove absence: %+v", absent)
	}
	parentOnly := linuxProof(id, func(name string) ([]byte, error) {
		switch name {
		case "/proc/self/stat":
			return procStatLine(os.Getpid(), 99, 10), nil
		case "/proc/99/stat":
			return procStatLine(99, 1, 10), nil
		default:
			return nil, os.ErrNotExist
		}
	}, sameNS)
	if !parentOnly.Gone || parentOnly.Proof != "pid_absent" {
		t.Fatalf("visible parent did not prove absence: %+v", parentOnly)
	}
}

func TestLinuxStartTicksDoNotUseWallClock(t *testing.T) {
	const liveTicks int64 = 5000
	read := func(string) ([]byte, error) { return procStatLine(42, 1, liveTicks), nil }
	ns := func() (uint64, error) { return 7, nil }
	base := processIdentity{PID: 42, StartedAt: time.Unix(1_700_000_000, 0).UTC(), PIDNamespaceIno: 7, StartTicks: liveTicks}
	alive := linuxProof(base, read, ns)
	if !alive.Alive || alive.Proof != "pid_alive" {
		t.Fatalf("matching ticks were not alive: %+v", alive)
	}
	reused := base
	reused.StartTicks = liveTicks - 10
	reused.StartedAt = time.Unix(1_800_000_000, 0).UTC()
	gone := linuxProof(reused, read, ns)
	if !gone.Gone || gone.Proof != "pid_reused" {
		t.Fatalf("later ticks were not pid reuse: %+v", gone)
	}
	old := base
	old.StartTicks = 0
	old.StartedAt = time.Unix(1, 0).UTC()
	unproven := linuxProof(old, read, ns)
	if unproven.Gone || unproven.Alive || unproven.Proof != "start_unproven" {
		t.Fatalf("missing launch ticks claimed pid reuse: %+v", unproven)
	}
}

func procStatLine(pid, ppid int, ticks int64) []byte {
	return []byte(fmt.Sprintf("%d (cmd) S %d 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d\n", pid, ppid, ticks))
}
