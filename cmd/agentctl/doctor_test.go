package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

type doctorDocument struct {
	OK     bool `json:"ok"`
	Result struct {
		Healthy    bool           `json:"healthy"`
		Supervisor map[string]any `json:"supervisor"`
		Problems   []string       `json:"problems"`
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
