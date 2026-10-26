package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ownerSubcommands are the agentctl invocations that own an execution for its lifetime. A binary or journal change
// under them can fail their next journal write, so updates wait for them (2026-10-25 m5 incident).
// dispatch and promote write the journal after remote work, so they count too.
var ownerSubcommands = map[string]bool{"delegate": true, "run": true, "fanout": true, "continue": true, "dispatch": true, "promote": true, "_delegate-worker": true}

// valueGlobalFlags take a separate value argument before the subcommand.
var valueGlobalFlags = map[string]bool{"--output": true, "--profile": true, "--context-file": true, "--config": true, "--config-bundle": true, "--journal": true, "--journal-busy-budget": true}

// ownerSubcommand returns the subcommand of an agentctl command line when it owns an execution, else "".
// names are the binary basenames that count as agentctl: "agentctl" plus the running executable's own name, since a
// managed install may use a custom name.
func ownerSubcommand(command string, names map[string]bool) string {
	fields := strings.Fields(command)
	if len(fields) == 0 || !names[filepath.Base(fields[0])] {
		return ""
	}
	for i := 1; i < len(fields); i++ {
		field := fields[i]
		if strings.HasPrefix(field, "-") {
			if valueGlobalFlags[field] {
				i++
			}
			continue
		}
		if ownerSubcommands[field] {
			return field
		}
		return ""
	}
	return ""
}

// ownerProcessIDs parses `ps -axo pid=,command=` and identifies owners other than self.
func ownerProcessIDs(psOutput []byte, self int, names map[string]bool) []string {
	owners := []string{}
	scanner := bufio.NewScanner(bytes.NewReader(psOutput))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		pidText, command, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid == self {
			continue
		}
		if ownerSubcommand(strings.TrimSpace(command), names) != "" {
			owners = append(owners, "pid:"+strconv.Itoa(pid))
		}
	}
	return owners
}

// liveOwnerProcesses counts running agentctl execution owners on this host.
func liveOwnerProcessIDs() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	return ownerProcessIDs(out, os.Getpid(), agentctlNames()), nil
}

// agentctlNames is "agentctl" plus this executable's basename (a managed install may be named differently).
func agentctlNames() map[string]bool {
	names := map[string]bool{"agentctl": true}
	if executable, err := os.Executable(); err == nil {
		names[filepath.Base(executable)] = true
	}
	return names
}

func countOwnerProcesses(out []byte, self int, names map[string]bool) int {
	return len(ownerProcessIDs(out, self, names))
}
