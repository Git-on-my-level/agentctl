package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func identityInvoke(t *testing.T, env map[string]string, args ...string) (int, identityReport, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.getenv = func(key string) string { return env[key] }
	a.updateNotice = func(context.Context, string, common) *output.Warning {
		t.Fatal("read-only identity ran automatic maintenance")
		return nil
	}
	code := a.run(context.Background(), append([]string{"identity", "--json"}, args...))
	var envelope struct {
		Result identityReport `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return code, envelope.Result, stdout.String()
}
func TestIdentityUnknownIsReadOnlyAndSecretFree(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "missing", "journal.db")
	secret := "fixture-credential-never-output"
	code, report, raw := identityInvoke(t, map[string]string{"OPENAI_API_KEY": secret, "ANTHROPIC_API_KEY": secret, "AGENTCTL_CONTEXT": secret}, "--journal", journal)
	if code != 0 || report.SchemaVersion != identitySchemaVersion || report.Provider.ID != nil || report.NativeSession.ID != nil || report.Execution.ID != nil || report.Environment.HostID != nil {
		t.Fatalf("unexpected unknown report: %s", raw)
	}
	for _, capability := range report.Capabilities {
		if capability.Status != "unknown" || capability.Confidence != "unknown" {
			t.Fatalf("invented capability: %#v", capability)
		}
	}
	if strings.Contains(raw, secret) || strings.Contains(raw, root) {
		t.Fatalf("private data in report: %s", raw)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("identity mutated state: %v %v", entries, err)
	}
}
func TestIdentityUnknownProviderAndStableNamespacedCorrelation(t *testing.T) {
	check := func(provider, session string) identityReport {
		code, report, raw := identityInvoke(t, nil, "--provider", provider, "--native-session-id", session)
		if code != 0 || report.Provider.ID == nil || report.NativeSession.ID == nil || report.Provider.Confidence != "self_reported" || strings.Contains(raw, session) {
			t.Fatalf("bad explicit identity: %s", raw)
		}
		if report.Capabilities["resume"].Status != "unknown" {
			t.Fatalf("unobserved provider capability: %s", raw)
		}
		return report
	}
	a := check("custom-agent", "conversation-one")
	b := check("custom-agent", "conversation-one")
	c := check("custom-agent", "conversation-two")
	d := check("another-agent", "conversation-one")
	if *a.NativeSession.ID != *b.NativeSession.ID || *a.NativeSession.ID == *c.NativeSession.ID || *a.NativeSession.ID == *d.NativeSession.ID {
		t.Fatal("correlation is not stable and provider scoped")
	}
	alias := check("claude-code", "conversation-one")
	canonical := check("claude", "conversation-one")
	if *alias.NativeSession.ID != *canonical.NativeSession.ID {
		t.Fatal("provider alias split the conversation")
	}
}
func TestIdentityNativeEnvironmentAndManagedParentIsolation(t *testing.T) {
	parent := "parent-conversation-fixture"
	code, report, raw := identityInvoke(t, map[string]string{"CODEX_THREAD_ID": parent})
	if code != 0 || identityDisplay(report.Provider) != "codex" || report.NativeSession.ID == nil || strings.Contains(raw, parent) {
		t.Fatalf("native hint failed: %s", raw)
	}
	for _, env := range []map[string]string{
		{"CODEX_THREAD_ID": parent, "AGENTCTL_EXECUTION_ID": "not-a-valid-execution"},
		{"CODEX_THREAD_ID": parent, "AGENTCTL_ADAPTER": "cursor"},
		{"CODEX_THREAD_ID": parent, "AGENTCTL_AUTHORITY": "native"},
	} {
		code, report, raw = identityInvoke(t, env)
		if code != 0 || report.NativeSession.ID != nil {
			t.Fatalf("managed child adopted parent conversation: %s", raw)
		}
	}
	code, report, raw = identityInvoke(t, map[string]string{"CODEX_THREAD_ID": parent, "AGENTCTL_ADAPTER": "cursor"}, "--provider", "custom-agent")
	if code != 0 || report.NativeSession.ID != nil || report.Execution.ID != nil || identityDisplay(report.Provider) != "custom-agent" {
		t.Fatalf("explicit provider borrowed ambient identity: %s", raw)
	}
}
func TestIdentityAvailabilityDoesNotClaimActiveProviderOrCapabilities(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	for _, name := range []string{"codex", "devin", "zcode"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	code, report, raw := identityInvoke(t, map[string]string{"PATH": root})
	if code != 0 || report.Provider.ID != nil || report.Capabilities["resume"].Status != "unknown" {
		t.Fatalf("availability asserted active capability: %s", raw)
	}
	found := map[string]string{}
	for _, h := range report.Harnesses {
		found[h.ProviderID] = h.Availability
	}
	if found["codex"] != "available" || found["devin"] != "available" || found["zcode"] != "available" || found["cursor"] != "unavailable" {
		t.Fatalf("bad inventory: %#v", found)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("identity executed a discovered binary")
	}
	if strings.Contains(raw, root) {
		t.Fatal("inventory leaked executable paths")
	}
}
func identityFixture(t *testing.T, journal *store.Journal, provider, session string) model.Execution {
	t.Helper()
	now := time.Now().UTC()
	value := model.Execution{Authority: model.AuthorityNative, Adapter: provider, Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "fixture", Items: []model.CapabilityItem{}}, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}}
	if session != "" {
		recordNativeSession(&value, adapter.SourceRef{Adapter: provider, Kind: "native_session", OpaqueID: session})
	}
	created, _, err := journal.CreateExecution(context.Background(), value, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	return created
}
func TestIdentityJournalCorrelatesAcrossExecutionsWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	first := identityFixture(t, journal, "codex", "private-conversation-fixture")
	second := identityFixture(t, journal, "codex", "private-conversation-fixture")
	journal.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	code, a, raw := identityInvoke(t, nil, "--journal", path, "--execution", first.ID.String())
	if code != 0 || a.NativeSession.ID == nil || a.Provider.Confidence != "observed" || a.Environment.HostID == nil || a.Capabilities["resume"].Status != "unsupported" {
		t.Fatalf("bad recorded identity: %s", raw)
	}
	code, b, raw := identityInvoke(t, map[string]string{"AGENTCTL_EXECUTION_ID": second.ID.String(), "AGENTCTL_ADAPTER": "cursor", "CODEX_THREAD_ID": "wrong-parent"}, "--journal", path)
	if code != 0 || b.NativeSession.ID == nil || *a.NativeSession.ID != *b.NativeSession.ID || *a.Execution.ID == *b.Execution.ID || identityDisplay(b.Provider) != "codex" {
		t.Fatalf("journal precedence/correlation failed: %s", raw)
	}
	if strings.Contains(raw, "private-conversation-fixture") || strings.Contains(raw, "wrong-parent") || strings.Contains(raw, path) {
		t.Fatal("identity leaked private source")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("identity wrote journal bytes")
	}
}
func TestIdentityCapabilitiesDoNotInventContinuation(t *testing.T) {
	execution := model.Execution{Authority: model.AuthorityNative, Adapter: "codex", State: model.StateCompleted}
	if got := identityExecutionCapabilities(execution)["resume"]; got.Status != "unsupported" || got.Reason != "frozen_launch_recipe_missing" {
		t.Fatal(got)
	}
	execution.Delegation = &model.DelegationBinding{NativePlan: &model.DelegationNativePlan{Argv: []string{"codex", "exec", "--json"}, PromptDelivery: "argv"}}
	if got := identityExecutionCapabilities(execution)["resume"]; got.Status != "unknown" {
		t.Fatal(got)
	}
	recordNativeSession(&execution, adapter.SourceRef{OpaqueID: "native-fixture"})
	if got := identityExecutionCapabilities(execution)["resume"]; got.Status != "supported" || got.Scope != "agentctl_continue" {
		t.Fatal(got)
	}
	execution.Authority = model.AuthorityMultica
	if got := identityExecutionCapabilities(execution)["resume"]; got.Status != "unsupported" || got.Reason != "authority_owns_continuation" {
		t.Fatal(got)
	}
}
func TestIdentityInvalidInputsFailWithoutEchoingValues(t *testing.T) {
	for _, args := range [][]string{{"--provider"}, {"--provider", "bad/provider"}, {"--native-session-id", "fixture-secret"}, {"--provider", "a", "--native-session-id", "fixture-secret\x00bad"}, {"--provider", "a", "--native-session-id", strings.Repeat("x", 257)}, {"--execution", "fixture-secret"}, {"--provider", "a", "--provider", "b"}} {
		code, _, raw := identityInvoke(t, nil, args...)
		if code != 2 || strings.Contains(raw, "fixture-secret") {
			t.Fatalf("invalid input output: %d %s", code, raw)
		}
	}
}
func TestNativeSessionBindingIdempotentAndRejectsProcessID(t *testing.T) {
	e := model.Execution{Adapter: "codex"}
	for _, ref := range []adapter.SourceRef{{OpaqueID: "42", PID: 42}, {OpaqueID: "bad\nvalue"}, {OpaqueID: strings.Repeat("x", 257)}} {
		if recordNativeSession(&e, ref) {
			t.Fatal("recorded invalid native ID")
		}
	}
	ref := adapter.SourceRef{OpaqueID: "fixture-session", PID: 42}
	if !recordNativeSession(&e, ref) || recordNativeSession(&e, ref) || len(e.SourceBindings) != 1 {
		t.Fatal("native ID recording is not idempotent")
	}
}
func TestIdentitySessionIsObservableBeforeNativeCompletion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state", "journal.db")
	script := filepath.Join(root, "codex")
	release := filepath.Join(root, "release")
	source := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.1.0'; exit 0; fi
printf '%s\n' '{"type":"thread.started","thread_id":"early-native-fixture"}'
while [ ! -e '` + release + `' ]; do sleep 0.02; done
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`
	if err := os.WriteFile(script, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	rawID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	done := make(chan int, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(func() { cancel(); <-finished })
	go func() {
		defer close(finished)
		done <- a.run(ctx, []string{"--journal", path, "run", "--adapter", "codex", "--execution-id", rawID.String(), "--", script, "exec", "--json"})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, report, _ := identityInvoke(t, nil, "--journal", path, "--execution", rawID.String())
		if code == 0 && report.NativeSession.ID != nil {
			if report.Capabilities["resume"].Status != "unsupported" {
				t.Fatal("expert run gained continuation")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native identity was not observed before child release")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("run failed: %s %s", stdout.String(), stderr.String())
	}
}

func TestIdentityCorrelationGoldenVector(t *testing.T) {
	got := sessionIdentity("codex", "conversation-1", "explicit", "self_reported")
	if got.ID == nil || *got.ID != "sha256:5860216ce9a5d395961ecf92a3059d42bc443d89faf558ce6d064b0be8330538" {
		t.Fatalf("correlation contract drift: %#v", got)
	}
}

func TestIdentityCompetingNativeEnvironmentIsUnknown(t *testing.T) {
	for _, marker := range []string{"CLAUDECODE", "CURSOR_AGENT_COMPLETED_PATH"} {
		env := map[string]string{"CODEX_THREAD_ID": "inherited-parent-conversation", marker: "private-marker-fixture"}
		code, report, raw := identityInvoke(t, env)
		if code != 0 || report.Provider.ID != nil || report.NativeSession.ID != nil || strings.Contains(raw, "private-marker-fixture") {
			t.Fatalf("ambiguous native environment adopted parent: %s", raw)
		}
		code, report, raw = identityInvoke(t, env, "--provider", "custom-agent", "--native-session-id", "explicit-session")
		if code != 0 || identityDisplay(report.Provider) != "custom-agent" || report.NativeSession.ID == nil || report.NativeSession.Confidence != "self_reported" {
			t.Fatalf("explicit identity was lost: %s", raw)
		}
	}
}
