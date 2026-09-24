package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteZCodeArgvKeepsExplicitPath(t *testing.T) {
	argv := []string{"/opt/zcode/bin/zcode", "--json", "--prompt", "hi"}
	got := rewriteZCodeArgv(argv)
	if len(got) != len(argv) || got[0] != argv[0] {
		t.Fatalf("explicit path rewritten: %#v", got)
	}
}

func TestZCodeParserReadsResponseDocument(t *testing.T) {
	raw := []byte(`{
  "sessionId": "sess_example",
  "response": "Hi!",
  "projection": {"status": "idle"}
}`)
	obs := zcodeParser{}.Parse(raw, false)
	if !obs.Terminal || !obs.Success || obs.Content != "Hi!" || obs.SessionID != "sess_example" {
		t.Fatalf("obs = %#v", obs)
	}
	if obs.ContentSource != "assistant_terminal_result" {
		t.Fatalf("source = %q", obs.ContentSource)
	}
}

func TestZCodeParserRejectsEmptyResponse(t *testing.T) {
	obs := zcodeParser{}.Parse([]byte(`{"sessionId":"sess_example","response":""}`), false)
	if obs.Success || obs.Data["diagnostic_code"] != "empty_terminal_result" {
		t.Fatalf("obs = %#v", obs)
	}
}

func TestZCodeProbeNamesMissingLauncherAndAppBundledCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	bundle := filepath.Join(t.TempDir(), "ZCode.app")
	entry := filepath.Join(bundle, "Contents", "Resources", "glm", "zcode.cjs")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("// cli\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := zcodeAppBundles
	zcodeAppBundles = func() []string { return []string{filepath.Join(t.TempDir(), "Absent.app"), bundle} }
	t.Cleanup(func() { zcodeAppBundles = previous })

	_, err := NewZCode().Probe(context.Background(), ProbeRequest{})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrDependencyUnavailable {
		t.Fatalf("err = %#v", err)
	}
	if adapterErr.Details["diagnostic_code"] != "zcode_launcher_missing" || adapterErr.Details["app_bundled_cli"] != entry {
		t.Fatalf("details = %#v", adapterErr.Details)
	}
	if remediation, _ := adapterErr.Details["remediation"].(string); !strings.Contains(remediation, "ELECTRON_RUN_AS_NODE=1") || !strings.Contains(remediation, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE") {
		t.Fatalf("remediation = %q", remediation)
	}
}

func TestZCodeProbeWithoutAppBundleStillNamesLauncher(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	previous := zcodeAppBundles
	zcodeAppBundles = func() []string { return []string{filepath.Join(t.TempDir(), "Absent.app")} }
	t.Cleanup(func() { zcodeAppBundles = previous })

	_, err := NewZCode().Probe(context.Background(), ProbeRequest{})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Details["diagnostic_code"] != "zcode_launcher_missing" {
		t.Fatalf("err = %#v", err)
	}
	if _, ok := adapterErr.Details["app_bundled_cli"]; ok {
		t.Fatalf("details = %#v", adapterErr.Details)
	}
}
