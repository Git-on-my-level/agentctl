package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cliBundle(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.json")
	document := `{"schema_version":1,"default_profile":"private","profiles":{"private":{"adapters":{"codex":{"executable":"/bin/echo"}}}}}`
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
	for _, command := range [][]string{{"config", "show"}, {"doctor", "--static"}} {
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		args := append([]string{"--config", base, "--config-bundle", bundle, "--profile", "private"}, command...)
		if code := a.run(context.Background(), args); code != 0 {
			t.Fatalf("%v exit=%d output=%s", command, code, stdout.String())
		}
		if !strings.Contains(stdout.String(), `"sha256"`) || !strings.Contains(stdout.String(), `"explicit_additive_bundle"`) {
			t.Fatalf("%v omitted bundle provenance/composition: %s", command, stdout.String())
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
