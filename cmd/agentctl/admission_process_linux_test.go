//go:build linux

package main

import (
	"os"
	"os/exec"
	"testing"
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
