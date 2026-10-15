package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

type probeRequestRecorder struct {
	adapter.Adapter
	request adapter.ProbeRequest
}

func (r *probeRequestRecorder) Probe(_ context.Context, request adapter.ProbeRequest) (adapter.ProbeResult, error) {
	r.request = request
	return adapter.ProbeResult{}, nil
}

func TestLaunchVersionProbeUsesManifestTimeout(t *testing.T) {
	tests := []struct {
		name    string
		runtime adapter.Adapter
		want    time.Duration
	}{
		{name: "codex", runtime: adapter.NewCodex(), want: 5 * time.Second},
		{name: "cursor", runtime: adapter.NewCursor(), want: 5 * time.Second},
		{name: "claude-code", runtime: adapter.NewClaudeCode(), want: 5 * time.Second},
		{name: "omp", runtime: adapter.NewOMP(), want: 5 * time.Second},
		{name: "zcode", runtime: adapter.NewZCode(), want: 30 * time.Second},
		{name: "devin", runtime: adapter.NewDevin(), want: 5 * time.Second},
		{name: "multica", runtime: adapter.NewMultica(adapter.MulticaConfig{Profile: "profile", Workspace: "workspace", Issue: "issue", Run: "run"}), want: 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := tt.runtime.Manifest()
			if len(manifest.ProbeActions) == 0 || manifest.ProbeActions[0].ID != "version" {
				t.Fatalf("manifest has no version probe action: %#v", manifest.ProbeActions)
			}
			if got := manifest.ProbeActions[0].TimeoutSeconds; got != int(tt.want/time.Second) {
				t.Fatalf("manifest timeout=%ds, want %s", got, tt.want)
			}

			recorder := &probeRequestRecorder{Adapter: tt.runtime}
			if _, err := probeVersion(context.Background(), recorder, adapter.ProbeRequest{}); err != nil {
				t.Fatal(err)
			}
			if recorder.request.Timeout != tt.want {
				t.Fatalf("runtime probe timeout=%s, want %s", recorder.request.Timeout, tt.want)
			}
			if !recorder.request.Fresh {
				t.Fatal("launch probe did not remain fresh")
			}
		})
	}
}

func TestSlowVersionProbeUsesInjectedPerAdapterBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow-version")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 0.6\nprintf 'fake-zcode 1.0\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	// These accelerated budgets preserve the production 5s/30s relationship
	// while keeping the test under a few seconds, even on a loaded host.
	zcodeBudget := 2 * time.Second
	zcode, err := probeVersionWithTimeout(context.Background(), adapter.NewZCode(), adapter.ProbeRequest{Executable: path}, zcodeBudget)
	if err != nil {
		t.Fatalf("ZCode slow version probe failed: %v", err)
	}
	if zcode.BackendVersion != "fake-zcode 1.0" {
		t.Fatalf("ZCode backend version=%q", zcode.BackendVersion)
	}

	otherBudget := 333 * time.Millisecond
	_, err = probeVersionWithTimeout(context.Background(), adapter.NewCodex(), adapter.ProbeRequest{Executable: path}, otherBudget)
	var adapterErr *adapter.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != adapter.ErrTimeout {
		t.Fatalf("Codex slow version probe error=%v, want timeout", err)
	}
	if adapterErr.Details["adapter"] != "codex" || adapterErr.Details["timeout"] != otherBudget.String() {
		t.Fatalf("timeout details=%#v", adapterErr.Details)
	}
	elapsedText, ok := adapterErr.Details["elapsed"].(string)
	if !ok {
		t.Fatalf("timeout elapsed detail has type %T", adapterErr.Details["elapsed"])
	}
	elapsed, parseErr := time.ParseDuration(elapsedText)
	if parseErr != nil || elapsed < otherBudget {
		t.Fatalf("elapsed=%q, parse error=%v, want at least %s", elapsedText, parseErr, otherBudget)
	}
	for _, phrase := range []string{"codex", otherBudget.String(), "elapsed", "host may be overloaded"} {
		if !strings.Contains(adapterErr.Error(), phrase) {
			t.Fatalf("timeout error %q omitted %q", adapterErr.Error(), phrase)
		}
	}
}

func TestCallerDeadlineIsNotReportedAsProbeBudgetTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow-version")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 2\nprintf 'fake-zcode 1.0\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := probeVersionWithTimeout(ctx, adapter.NewZCode(), adapter.ProbeRequest{Executable: path}, 30*time.Second)
	var adapterErr *adapter.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != adapter.ErrTimeout {
		t.Fatalf("caller deadline error=%v, want timeout", err)
	}
	if adapterErr.Details["diagnostic_code"] == "native_version_probe_timeout" || strings.Contains(adapterErr.Error(), "host may be overloaded") {
		t.Fatalf("caller deadline reported as probe budget expiry: %v %#v", adapterErr, adapterErr.Details)
	}
}
