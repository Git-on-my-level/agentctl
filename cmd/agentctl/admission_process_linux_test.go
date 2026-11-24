//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/admission"
)

func TestAdmissionLinuxProcView(t *testing.T) {
	for _, tc := range []struct {
		status string
		valid  bool
	}{
		{"Name:\tagentctl\nNSpid:\t42\n", true},
		{"NSpid:\t42\t42\n", false},
		{"NSpid:\t8192\t42\n", false},
		{"NSpid:\t43\n", false},
		{"Pid:\t42\n", false},
	} {
		if err := validateAdmissionProcView(tc.status, 42); (err == nil) != tc.valid {
			t.Errorf("validate %q: %v", tc.status, err)
		}
	}
}

func TestNativeLeaseReleasesUnreapedLinuxChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	generation, err := admissionProcessStart(child.Process.Pid)
	if err != nil || generation == "" {
		t.Fatalf("child generation: %q %v", generation, err)
	}
	slots := admission.NewWithChildLiveness(filepath.Join(t.TempDir(), "journal.db"), time.Now, admissionProcessStart, admissionLiveChildStart)
	b := admission.Bucket{Harness: "opencode", Model: "morph/flash", MinWindow: 1, InitialWindow: 1, MaxWindow: 1}
	self, err := admissionProcessStart(os.Getpid())
	if err != nil || self == "" {
		t.Fatalf("owner generation: %q %v", self, err)
	}
	owner := admission.Owner{PID: os.Getpid(), Start: self}
	native := slots.Queue("morph", b, "native", admission.Background, owner, admission.Owner{}, false)
	defer native.Close()
	if d, err := native.Try(ctx, false); err != nil || !d.Granted {
		t.Fatalf("native grant: %+v %v", d, err)
	}
	if err := native.PrepareLaunch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	for {
		live, err := admissionLiveChildStart(child.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if live == "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("unreaped child was still reported alive")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := native.BindChild(child.Process.Pid); err != nil {
		t.Fatalf("binding a fast-exiting child: %v", err)
	}
	native.Close()
	if start, err := admissionProcessStart(child.Process.Pid); err != nil || start != generation {
		t.Fatalf("unreaped child generation: %q %v", start, err)
	}
	contender := slots.Queue("morph", b, "contender", admission.Interactive, owner, admission.Owner{}, false)
	defer contender.Close()
	if d, err := contender.Try(ctx, false); err != nil || !d.Granted {
		t.Fatalf("exited child retained its unlocked lease: %+v %v", d, err)
	}
}

func TestAdmissionLinuxProcessIdentityIsStableAndReclaimable(t *testing.T) {
	pid := os.Getpid()
	self, err := admissionProcessStart(pid)
	if err != nil || self == "" {
		t.Fatalf("self identity: %q %v", self, err)
	}
	if again, err := admissionProcessStart(pid); err != nil || again != self {
		t.Fatalf("self identity changed: %q %v", again, err)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	pid = child.Process.Pid
	if start, err := admissionProcessStart(pid); err != nil || start == "" {
		t.Fatalf("child identity: %q %v", start, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if start, err := admissionProcessStart(pid); err != nil || start != "" {
		t.Fatalf("exited child: %q %v", start, err)
	}
}
