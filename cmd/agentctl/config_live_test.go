package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestConfigDoctorLiveReportsCatalogAvailability(t *testing.T) {
	root := testutil.TempDir(t)
	codex := filepath.Join(root, "codex")
	marker := filepath.Join(root, "probed")
	body := "#!/bin/sh\ntouch '" + marker + "'\necho '{\"models\":[{\"slug\":\"gpt-6-luna\",\"supported_reasoning_levels\":[{\"effort\":\"xhigh\"}]}]}'\n"
	if err := os.WriteFile(codex, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(model string) string {
		raw, err := json.Marshal(map[string]any{"schema_version": 1, "default_profile": "p", "profiles": map[string]any{"p": map[string]any{
			"adapters":          map[string]any{"codex": map[string]any{"executable": codex}},
			"agent_preferences": map[string]any{"mode": "advisory", "preferred": []any{map[string]any{"agent": "codex", "model": model, "speed": "regular", "effort": "xhigh"}}},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, model+".json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	run := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := testApp(&stdout, &stderr).run(context.Background(), args)
		return code, stdout.String()
	}

	if code, out := run("--config", write("gpt-6-luna"), "config", "doctor"); code != 0 || strings.Contains(out, "live_catalog") {
		t.Fatalf("plain doctor exit=%d %s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("plain doctor started a native CLI")
	}
	if code, out := run("--config", write("gpt-6-luna"), "config", "doctor", "--live"); code != 0 || !strings.Contains(out, `"live_catalog":{"status":"available"`) {
		t.Fatalf("available exit=%d %s", code, out)
	}
	if code, out := run("--config", write("gpt-retired"), "config", "doctor", "--live"); code != 6 || !strings.Contains(out, `"dependency_unavailable"`) || !strings.Contains(out, `"model_unavailable"`) {
		t.Fatalf("unavailable exit=%d %s", code, out)
	}
	if code, _ := run("--config", write("gpt-6-luna"), "config", "validate", "--live"); code == 0 {
		t.Fatal("validate accepted --live")
	}
}
