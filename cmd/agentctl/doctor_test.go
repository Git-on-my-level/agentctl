package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

type doctorDocument struct {
	OK     bool `json:"ok"`
	Result struct {
		Healthy    bool           `json:"healthy"`
		Supervisor map[string]any `json:"supervisor"`
		Adapters   []struct {
			Name         string                    `json:"name"`
			Status       string                    `json:"status"`
			Capabilities map[string]map[string]any `json:"capabilities"`
		} `json:"adapters"`
		Problems []string `json:"problems"`
	} `json:"result"`
	NextActions []output.NextAction `json:"next_actions"`
}

func runDoctorWithSupervisorHealth(t *testing.T, probe func(context.Context) (supervisor.Status, error), now time.Time) doctorDocument {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.now = func() time.Time { return now }
	a.supervisorHealthProbe = probe
	journalPath := filepath.Join(home, "state", "journal.db")
	configPath := filepath.Join(home, "config.json")
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	if problem := a.doctorReadiness(context.Background(), renderer, common{mode: output.JSON, journalPath: journalPath, configPath: configPath}, []string{"--adapter", "generic-process"}); problem != nil {
		t.Fatalf("doctor failed: %v", problem)
	}
	var document doctorDocument
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("doctor output %q: %v", stdout.String(), err)
	}
	return document
}

func writeDoctorMulticaFixture(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "fake-multica")
	body := `#!/bin/sh
case "$*" in
  *"--version"*) printf '%s\n' 'multica 0.0.0-doctor-test' ;;
  *"workspace get"*) printf '%s\n' '{"id":"workspace-test"}' ;;
  *) printf 'unexpected argv: %s\n' "$*" >&2; exit 9 ;;
esac
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func runDoctorWithMultica(t *testing.T, probe func(context.Context) (supervisor.Status, error), now time.Time, args ...string) doctorDocument {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake := writeDoctorMulticaFixture(t, home)
	configPath := filepath.Join(home, "config", "config.json")
	configJSON := `{"schema_version":1,"default_profile":"fleet","profiles":{"fleet":{"multica":{"executable":"` + fake + `","profile":"desktop","workspace_id":"workspace-test","server_url":"https://multica.example.test","app_url":"https://multica.example.test"}}}}`
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.now = func() time.Time { return now }
	a.supervisorHealthProbe = probe
	journalPath := filepath.Join(home, "state", "journal.db")
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	if problem := a.doctorReadiness(context.Background(), renderer, common{mode: output.JSON, journalPath: journalPath, configPath: configPath}, args); problem != nil {
		t.Fatalf("doctor failed: %v", problem)
	}
	var document doctorDocument
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("doctor output %q: %v", stdout.String(), err)
	}
	return document
}

func doctorAdapterStatus(document doctorDocument, name string) (string, map[string]map[string]any) {
	for _, value := range document.Result.Adapters {
		if value.Name == name {
			return value.Status, value.Capabilities
		}
	}
	return "", nil
}

func containsProblem(problems []string, want string) bool {
	for _, problem := range problems {
		if problem == want {
			return true
		}
	}
	return false
}

func hasSupervisorStatusAction(actions []output.NextAction) bool {
	for _, action := range actions {
		if len(action.Argv) == 3 && action.Argv[1] == "supervisor" && action.Argv[2] == "status" {
			return true
		}
	}
	return false
}

func TestDoctorReportsDegradedSupervisorAsUnhealthy(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	document := runDoctorWithSupervisorHealth(t, func(context.Context) (supervisor.Status, error) {
		return supervisor.Status{
			Running:    true,
			SocketPath: "/owner-only/supervisor.sock",
			Health: supervisor.Health{
				State:             supervisor.HealthDegraded,
				LastCycleAt:       now.Add(-5 * time.Second),
				PendingDeliveries: 11,
				LastError:         "stored execution has no native source binding",
			},
			Now: now,
		}, nil
	}, now)

	if document.Result.Healthy {
		t.Fatalf("degraded supervisor reported healthy: %#v", document.Result)
	}
	block := document.Result.Supervisor
	if block["status"] != "degraded" || block["health_state"] != "degraded" {
		t.Fatalf("supervisor block hid the degraded state: %#v", block)
	}
	if block["pending_deliveries"] != float64(11) {
		t.Fatalf("supervisor block omitted pending deliveries: %#v", block)
	}
	if block["last_error"] != "stored execution has no native source binding" {
		t.Fatalf("supervisor block omitted the last error: %#v", block)
	}
	if block["cycle_freshness"] != "fresh" {
		t.Fatalf("recent cycle was not reported fresh: %#v", block)
	}
	if !containsProblem(document.Result.Problems, "supervisor_degraded") {
		t.Fatalf("problems=%v", document.Result.Problems)
	}
	if !hasSupervisorStatusAction(document.NextActions) {
		t.Fatalf("doctor offered no supervisor next action: %#v", document.NextActions)
	}
}

func TestDoctorReportsStaleSupervisorCycleAsUnhealthy(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	document := runDoctorWithSupervisorHealth(t, func(context.Context) (supervisor.Status, error) {
		return supervisor.Status{
			Running:    true,
			SocketPath: "/owner-only/supervisor.sock",
			Health:     supervisor.Health{State: supervisor.HealthHealthy, LastCycleAt: now.Add(-30 * time.Minute)},
			Now:        now,
		}, nil
	}, now)

	if document.Result.Healthy {
		t.Fatalf("stale supervisor cycle reported healthy: %#v", document.Result)
	}
	if document.Result.Supervisor["cycle_freshness"] != "stale" {
		t.Fatalf("supervisor block=%#v", document.Result.Supervisor)
	}
	if !containsProblem(document.Result.Problems, "supervisor_cycle_stale") {
		t.Fatalf("problems=%v", document.Result.Problems)
	}
	if !hasSupervisorStatusAction(document.NextActions) {
		t.Fatalf("doctor offered no supervisor next action: %#v", document.NextActions)
	}
}

func TestDoctorKeepsHealthySupervisorSilent(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	document := runDoctorWithSupervisorHealth(t, func(context.Context) (supervisor.Status, error) {
		return supervisor.Status{
			Running:    true,
			SocketPath: "/owner-only/supervisor.sock",
			Health:     supervisor.Health{State: supervisor.HealthHealthy, LastCycleAt: now.Add(-2 * time.Second)},
			Now:        now,
		}, nil
	}, now)

	block := document.Result.Supervisor
	if block["status"] != "healthy" || block["cycle_freshness"] != "fresh" {
		t.Fatalf("supervisor block=%#v", block)
	}
	for _, problem := range document.Result.Problems {
		if len(problem) >= len("supervisor_") && problem[:len("supervisor_")] == "supervisor_" {
			t.Fatalf("healthy supervisor produced problem %q", problem)
		}
	}
	if hasSupervisorStatusAction(document.NextActions) {
		t.Fatalf("healthy supervisor still recommended an inspection: %#v", document.NextActions)
	}
}

func TestDoctorReportsUnreachableSupervisorSocket(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	document := runDoctorWithSupervisorHealth(t, func(context.Context) (supervisor.Status, error) {
		return supervisor.Status{}, errors.New("dial unix supervisor.sock: connect: connection refused")
	}, now)

	if document.Result.Healthy {
		t.Fatalf("unreachable supervisor reported healthy: %#v", document.Result)
	}
	if document.Result.Supervisor["status"] != "unreachable" {
		t.Fatalf("supervisor block=%#v", document.Result.Supervisor)
	}
	if !containsProblem(document.Result.Problems, "supervisor_unreachable") {
		t.Fatalf("problems=%v", document.Result.Problems)
	}
}

func TestDoctorReportsMulticaReadyWithSnapshotOnlyObservation(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	document := runDoctorWithMultica(t, func(context.Context) (supervisor.Status, error) {
		return supervisor.Status{}, nil
	}, now, "--adapter", "multica")

	status, capabilities := doctorAdapterStatus(document, "multica")
	if status != "ready" {
		t.Fatalf("multica adapter status=%q problems=%v", status, document.Result.Problems)
	}
	if containsProblem(document.Result.Problems, "multica_delegation_unavailable") {
		t.Fatalf("problems=%v", document.Result.Problems)
	}
	snapshot, ok := capabilities["snapshot"]
	if !ok {
		t.Fatalf("multica summary omitted snapshot: %#v", capabilities)
	}
	if snapshot["status"] == "unavailable" {
		t.Fatalf("multica snapshot reported unavailable after a verified workspace get: %#v", capabilities)
	}
	events, ok := capabilities["events"]
	if !ok || events["status"] != "unavailable" {
		t.Fatalf("multica summary must keep events unavailable visible: %#v", capabilities)
	}
}

func TestDoctorMulticaDelegationUnavailableWithoutObservation(t *testing.T) {
	probe := adapter.ProbeResult{Capabilities: []adapter.Capability{
		{Name: adapter.CapabilitySnapshot, Status: adapter.CapabilityUnavailable},
		{Name: adapter.CapabilityEvents, Status: adapter.CapabilityUnavailable},
	}}
	if doctorProbeReady("multica", probe) {
		t.Fatal("multica probe ready with snapshot and events both unavailable")
	}
	manifest := adapter.Manifest{Capabilities: []adapter.CapabilityDeclaration{
		{Name: adapter.CapabilitySnapshot, Implementation: adapter.CapabilityUnavailable},
		{Name: adapter.CapabilityEvents, Implementation: adapter.CapabilityUnavailable},
	}}
	if doctorManifestReady("multica", manifest) {
		t.Fatal("multica manifest ready with snapshot and events both unavailable")
	}
	for _, value := range []adapter.CapabilityStatus{adapter.CapabilityDegraded, adapter.CapabilitySupported} {
		probe.Capabilities[0].Status = value
		if !doctorProbeReady("multica", probe) {
			t.Fatalf("multica probe not ready with snapshot status %q", value)
		}
		manifest.Capabilities[0].Implementation = adapter.CapabilityConditional
		if value == adapter.CapabilitySupported {
			manifest.Capabilities[0].Implementation = adapter.CapabilitySupported
		}
		if !doctorManifestReady("multica", manifest) {
			t.Fatalf("multica manifest not ready with snapshot implementation %q", manifest.Capabilities[0].Implementation)
		}
	}
	eventsOnly := adapter.ProbeResult{Capabilities: []adapter.Capability{
		{Name: adapter.CapabilitySnapshot, Status: adapter.CapabilityUnavailable},
		{Name: adapter.CapabilityEvents, Status: adapter.CapabilitySupported},
	}}
	if !doctorProbeReady("multica", eventsOnly) {
		t.Fatal("multica probe not ready with events still usable")
	}
}

func TestDoctorReportsDegradedSupervisorAlongsideReadyMultica(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	document := runDoctorWithMultica(t, func(context.Context) (supervisor.Status, error) {
		return supervisor.Status{
			Running:    true,
			SocketPath: "/owner-only/supervisor.sock",
			Health: supervisor.Health{
				State:       supervisor.HealthDegraded,
				LastCycleAt: now.Add(-5 * time.Second),
				LastError:   "Multica event list did not return a structured page",
			},
			Now: now,
		}, nil
	}, now, "--adapter", "multica")

	status, _ := doctorAdapterStatus(document, "multica")
	if status != "ready" {
		t.Fatalf("multica adapter status=%q problems=%v", status, document.Result.Problems)
	}
	if document.Result.Healthy {
		t.Fatal("degraded supervisor reported healthy beside a ready Multica adapter")
	}
	if !containsProblem(document.Result.Problems, "supervisor_degraded") {
		t.Fatalf("problems=%v", document.Result.Problems)
	}
	if document.Result.Supervisor["status"] != "degraded" {
		t.Fatalf("supervisor block=%#v", document.Result.Supervisor)
	}
}
