package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/config"
)

func cliBundle(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.json")
	document := `{"schema_version":1,"default_profile":"private","profiles":{"private":{"adapters":{"codex":{"executable":"/bin/echo"}},"agent_preferences":{"mode":"advisory","preferred":[{"agent":"cursor","model":"composer-2.5","speed":"regular","use_for":"default"}],"notes":["Never select fast variants."]}}}}`
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigBundlePlanIsExplicitReadOnlyAndReportsDigest(t *testing.T) {
	bundle := cliBundle(t)
	base := filepath.Join(t.TempDir(), "missing-config.json")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	args := []string{"--config", base, "--config-bundle", bundle, "config", "bundle", "plan"}
	if code := a.run(context.Background(), args); code != 0 {
		t.Fatalf("plan exit=%d output=%s", code, stdout.String())
	}
	var document struct {
		Result struct {
			Valid      bool `json:"valid"`
			Mutates    bool `json:"mutates"`
			Provenance struct {
				SHA256 string `json:"sha256"`
			} `json:"provenance"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if !document.Result.Valid || document.Result.Mutates || document.Result.Provenance.SHA256 == "" {
		t.Fatalf("unexpected plan: %s", stdout.String())
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("plan created base config: %v", err)
	}
}

func TestConfigShowAndDoctorExposeBundleComposition(t *testing.T) {
	bundle := cliBundle(t)
	base := filepath.Join(t.TempDir(), "missing-config.json")
	for _, command := range [][]string{{"config", "show"}, {"config", "doctor"}, {"doctor", "--static"}} {
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		args := append([]string{"--config", base, "--config-bundle", bundle, "--profile", "private"}, command...)
		if code := a.run(context.Background(), args); code != 0 {
			t.Fatalf("%v exit=%d output=%s", command, code, stdout.String())
		}
		if !strings.Contains(stdout.String(), `"sha256"`) || !strings.Contains(stdout.String(), `"explicit_additive_bundle"`) {
			t.Fatalf("%v omitted bundle provenance/composition: %s", command, stdout.String())
		}
		if !strings.Contains(stdout.String(), `"agent_preferences"`) || !strings.Contains(stdout.String(), `"composer-2.5"`) {
			t.Fatalf("%v omitted advisory agent preferences: %s", command, stdout.String())
		}
	}
}

func TestConfigBundleIsNeverImplicitlyDiscovered(t *testing.T) {
	bundle := cliBundle(t)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	base := filepath.Join(t.TempDir(), "missing-config.json")
	a.getenv = func(name string) string {
		if name == "AGENTCTL_CONFIG_BUNDLE" {
			return bundle
		}
		return ""
	}
	if code := a.run(context.Background(), []string{"--config", base, "config", "show"}); code == 0 || !strings.Contains(stdout.String(), `"code":"not_found"`) {
		t.Fatalf("implicit bundle discovery occurred: exit=%d output=%s", code, stdout.String())
	}
}

func TestConfigSetProfilePreservesReviewedAgentPreferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	preferences := &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: "cursor", Model: "composer-2.5", Speed: "regular"}}, Notes: []string{"Never fast."}}
	cfg := config.Config{SchemaVersion: config.SchemaVersion, DefaultProfile: "fleet", Profiles: map[string]config.Profile{"fleet": {Adapters: map[string]config.Adapter{"cursor": {Executable: "/old/cursor"}}, AgentPreferences: preferences}}}
	if err := config.Save(path, cfg, false); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--config", path, "config", "set-profile", "--name", "fleet", "--adapter", "cursor=/new/cursor", "--replace"}); code != 0 {
		t.Fatalf("set-profile exit=%d output=%s", code, stdout.String())
	}
	updated, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	profile := updated.Profiles["fleet"]
	if profile.Adapters["cursor"].Executable != "/new/cursor" || profile.AgentPreferences == nil || profile.AgentPreferences.Preferred[0].Model != "composer-2.5" {
		t.Fatalf("set-profile did not preserve preferences: %#v", profile)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--config", path, "config", "set-profile", "--name", "fleet", "--adapter", "cursor=/new/cursor"}); code != 0 {
		t.Fatalf("idempotent set-profile exit=%d output=%s", code, stdout.String())
	}
}

func TestConfigSourceInitPlanIsNetworkFreeAndReadOnly(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "runtime", "config.json")
	checkoutPath := filepath.Join(root, "checkout")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	code := a.run(context.Background(), []string{"--config", configPath, "config", "source", "init", "--remote", "git@github.com:example/private-config.git", "--checkout", checkoutPath, "--plan"})
	if code != 0 {
		t.Fatalf("source plan exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"plan_invocation_side_effects":{"network_access":false,"mutates":false}`) || !strings.Contains(stdout.String(), `"apply_invocation_side_effects":{"network_access":true,"mutates":true}`) || !strings.Contains(stdout.String(), `"remote_validated":false`) {
		t.Fatalf("source plan omitted safety contract: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Dir(configPath)); !os.IsNotExist(err) {
		t.Fatalf("source plan created config parent: %v", err)
	}
	if _, err := os.Stat(checkoutPath); !os.IsNotExist(err) {
		t.Fatalf("source plan created checkout: %v", err)
	}
}

func TestConfigSourceStatusIsExplicitlyUnconfigured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	configPath := filepath.Join(t.TempDir(), "missing", "config.json")
	if code := a.run(context.Background(), []string{"--config", configPath, "config", "source", "status"}); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"configured":false`) || !strings.Contains(stdout.String(), `"in_sync":true`) {
		t.Fatalf("unexpected source status: %s", stdout.String())
	}
}

func TestSourceRestorePlanDistinguishesLiveDriftFromCheckoutDrift(t *testing.T) {
	tests := []struct {
		name       string
		status     config.SourceStatus
		want       bool
		wantBlocks int
	}{
		{name: "unconfigured", status: config.SourceStatus{}, want: false},
		{name: "live only", status: config.SourceStatus{Configured: true, Drift: []string{"live_config"}}, want: true},
		{name: "checkout only", status: config.SourceStatus{Configured: true, Drift: []string{"checkout_dirty"}}, wantBlocks: 1},
		{name: "live and checkout", status: config.SourceStatus{Configured: true, Drift: []string{"live_config", "checkout_revision"}}, wantBlocks: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, blockers := sourceRestorePlan(test.status)
			if got != test.want || len(blockers) != test.wantBlocks {
				t.Fatalf("would_restore=%v blockers=%v", got, blockers)
			}
		})
	}
}
