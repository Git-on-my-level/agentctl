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
func ownerSubcommand(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 || filepath.Base(fields[0]) != "agentctl" {
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

// countOwnerProcesses parses `ps -axo pid=,command=` output and counts execution owners other than self.
func countOwnerProcesses(psOutput []byte, self int) int {
	count := 0
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
		if ownerSubcommand(strings.TrimSpace(command)) != "" {
			count++
		}
	}
	return count
}

// liveOwnerProcesses counts running agentctl execution owners on this host.
func liveOwnerProcesses() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return 0, err
	}
	return countOwnerProcesses(out, os.Getpid()), nil
}
