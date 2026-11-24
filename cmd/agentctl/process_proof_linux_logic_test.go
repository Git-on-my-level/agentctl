package main

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

func TestLinuxPresentPIDRequiresMatchingNamespace(t *testing.T) {
	for _, ticks := range []int64{100, 200} {
		for _, scenario := range []string{"mismatch", "unrecorded", "unattested", "wrong_procfs", "unknown", "error", "no_getter"} {
			t.Run(fmt.Sprintf("%d/%s", ticks, scenario), func(t *testing.T) {
				id := processIdentity{PID: 42, StartTicks: 100, PIDNamespaceIno: 7, ProcfsPIDNamespaceIno: 7}
				getter := func() (uint64, error) { return 8, nil }
				switch scenario {
				case "unrecorded":
					id.PIDNamespaceIno = 0
					getter = func() (uint64, error) { return 7, nil }
				case "unattested":
					id.ProcfsPIDNamespaceIno = 0
					getter = func() (uint64, error) { return 7, nil }
				case "wrong_procfs":
					id.ProcfsPIDNamespaceIno = 8
					getter = func() (uint64, error) { return 7, nil }
				case "unknown":
					getter = func() (uint64, error) { return 0, nil }
				case "error":
					getter = func() (uint64, error) { return 0, errors.New("namespace unavailable") }
				case "no_getter":
					getter = nil
				}
				reads := 0
				proof := linuxProof(id, func(string) ([]byte, error) {
					reads++
					return procStatLine(42, 1, ticks), nil
				}, getter)
				if proof.Alive || proof.Gone || proof.Proof != "unproven" || reads != 0 {
					t.Fatalf("used an aliased PID without namespace proof: %+v reads=%d", proof, reads)
				}
			})
		}
	}
}

func TestLinuxInheritedProcfsCannotProveMissingOrReusedPID(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			id := processIdentity{PID: 42, StartTicks: 100, PIDNamespaceIno: 7, ProcfsPIDNamespaceIno: 7}
			reads := 0
			proof := linuxProof(id, func(string) ([]byte, error) {
				reads++
				if missing {
					return nil, os.ErrNotExist
				}
				return procStatLine(42, 1, 200), nil
			}, func() (uint64, error) {
				return adapter.ProcPIDNamespace(func(path string) (uint64, error) {
					if path == "/proc/self/ns/pid" {
						return 7, nil
					}
					return 8, nil // procfs belongs to an ancestor namespace
				})
			})
			if proof.Gone || proof.Alive || proof.Proof != "unproven" || reads != 0 {
				t.Fatalf("inherited procfs authorized a PID lookup: %+v reads=%d", proof, reads)
			}
		})
	}
}

func TestLinuxENOENTWithoutNamespaceProofIsUnproven(t *testing.T) {
	read := func(name string) ([]byte, error) {
		if name == "/proc/1/stat" {
			return procStatLine(1, 0, 10), nil
		}
		return nil, os.ErrNotExist
	}
	sameNS := func() (uint64, error) { return 7, nil }
	otherNS := func() (uint64, error) { return 8, nil }
	id := processIdentity{PID: 42, StartedAt: time.Unix(1, 0).UTC(), StartTicks: 100, PIDNamespaceIno: 7, ProcfsPIDNamespaceIno: 7}

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
	base := processIdentity{PID: 42, StartedAt: time.Unix(1_700_000_000, 0).UTC(), PIDNamespaceIno: 7, ProcfsPIDNamespaceIno: 7, StartTicks: liveTicks}
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

func TestLinuxQueuedProcessStartRetriesWithoutPIDOneAccess(t *testing.T) {
	const pid = 42
	const ticks = 5000
	calls, retries := 0, 0
	read := func(path string) ([]byte, error) {
		switch path {
		case "/proc/self/status":
			return []byte("Name:\tagentctl\nNSpid:\t123\n"), nil
		case "/proc/42/stat":
			calls++
			if calls == 1 {
				return nil, os.ErrNotExist
			}
			return procStatLine(pid, 123, ticks), nil
		default:
			t.Fatalf("unexpected procfs read %s", path)
			return nil, os.ErrPermission
		}
	}
	ownView := func() (uint64, error) {
		return linuxOwnProcNamespace(123, read, func(path string) (uint64, error) {
			if path != "/proc/self/ns/pid" {
				t.Fatalf("unexpected namespace lookup %s", path)
			}
			return 7, nil
		})
	}
	got := linuxProcessStart(pid, read, ownView, func() { retries++ })
	if !got.present || !got.startKnown || got.startTicks != ticks || got.nsInode != 7 || !got.started.IsZero() || got.err != nil || retries != 1 {
		t.Fatalf("queued identity = %+v after %d retries", got, retries)
	}
	id := processIdentity{PID: pid, StartTicks: got.startTicks, PIDNamespaceIno: got.nsInode, ProcfsPIDNamespaceIno: got.nsInode, exactStart: true}
	if proof := linuxProof(id, read, ownView); !proof.Alive {
		t.Fatalf("matching queued worker = %+v", proof)
	}
	id.StartTicks++
	if proof := linuxProof(id, read, ownView); !proof.Gone || proof.Proof != "pid_reused" {
		t.Fatalf("reused queued worker PID = %+v", proof)
	}
}

func TestLinuxAdmissionChildLivenessDistinguishesExitedState(t *testing.T) {
	for _, state := range []byte{'S', 'T', 'Z', 'X', 'x'} {
		t.Run(string(state), func(t *testing.T) {
			read := func(path string) ([]byte, error) {
				if path != "/proc/42/stat" {
					t.Fatalf("unexpected read of %s", path)
				}
				return []byte(fmt.Sprintf("42 (opencode (task)) %c 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 5000\n", state)), nil
			}
			absent := func(int) bool {
				t.Fatal("readable procfs stat was checked for absence")
				return false
			}
			want := "boot/7/5000"
			if generation, err := linuxAdmissionProcessStart(42, "boot/7", false, read, absent); err != nil || generation != want {
				t.Fatalf("bind generation=%q err=%v", generation, err)
			}
			if state == 'Z' || state == 'X' || state == 'x' {
				want = ""
			}
			if generation, err := linuxAdmissionProcessStart(42, "boot/7", true, read, absent); err != nil || generation != want {
				t.Fatalf("live lease generation=%q err=%v, want %q", generation, err, want)
			}
		})
	}
}

func procStatLine(pid, ppid int, ticks int64) []byte {
	return []byte(fmt.Sprintf("%d (cmd) S %d 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d\n", pid, ppid, ticks))
}
