package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

func writeZCodeEffort(t *testing.T, path, level string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body := `{"schemaVersion":1,"config":{"providerConfigRules":{"secret":"sentinel-credential"},"defaultModelSelection":{"providerId":"zai","modelId":"glm-5.3","options":{"reasoningLevel":"` + level + `"}}}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveZCodeEffortSelectedProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, ".zcode/cli/provider_config.json")
	flash := filepath.Join(home, ".zcode/cli/models/glm-5.3-flash/provider_config.json")
	override := filepath.Join(home, "override.json")
	writeZCodeEffort(t, base, "high")
	writeZCodeEffort(t, flash, "max")
	writeZCodeEffort(t, override, "low")
	for _, tc := range []struct {
		name, selector, override, requested, want string
		readOnly, mismatch                        bool
	}{
		{"base", "", "", "high", "high", false, false},
		{"flash", "glm-5.3-flash", "", "", "max", false, false},
		{"flash_mismatch", "glm-5.3-flash", "", "high", "max", false, true},
		{"override", "", override, "", "low", false, false},
		{"relative_override", "", "override.json", "", "low", false, false},
		{"flash_ignores_override", "glm-5.3-flash", override, "", "max", false, false},
		{"readonly_ignores_override", "", override, "high", "high", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZCODE_PERSONAL_PROVIDER_CONFIG_FILE", tc.override)
			t.Setenv("ZCODE_MODEL", "glm-5.3-flash") // recipe must win
			env := []string{"ZCODE_MODEL=" + tc.selector}
			if tc.readOnly {
				env = append(env, launchrecipe.ZCodeReadOnlyProfileEnv)
			}
			settings := model.DelegationSettings{Effort: "high"}
			problem := resolveZCodeEffort(&settings, env, tc.requested, home)
			if (problem != nil) != tc.mismatch || settings.Effort != tc.want || settings.EffortWarning != "" {
				t.Fatalf("settings=%+v problem=%v", settings, problem)
			}
			out, _ := json.Marshal([]any{settings, problem})
			if strings.Contains(string(out), "sentinel-credential") || strings.Contains(string(out), home) {
				t.Fatal("config data leaked")
			}
		})
	}
	t.Setenv("ZCODE_PERSONAL_PROVIDER_CONFIG_FILE", "")
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	settings := model.DelegationSettings{Effort: "high"}
	if p := resolveZCodeEffort(&settings, []string{"ZCODE_MODEL="}, "", home); p != nil || settings.Effort != "" || settings.EffortWarning == "" {
		t.Fatalf("unknown=%+v %v", settings, p)
	}
	if p := resolveZCodeEffort(&settings, []string{"ZCODE_MODEL="}, "high", home); p == nil {
		t.Fatal("unknown must refuse required effort")
	}
}

func TestResolveZCodeEffortUnsupportedLevels(t *testing.T) {
	for _, level := range []string{"medium", "xhigh", "ultra"} {
		t.Run(level, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("ZCODE_PERSONAL_PROVIDER_CONFIG_FILE", "")
			writeZCodeEffort(t, filepath.Join(home, ".zcode/cli/provider_config.json"), level)
			for _, required := range []string{"", level, "high"} {
				settings := model.DelegationSettings{Effort: level}
				problem := resolveZCodeEffort(&settings, []string{"ZCODE_MODEL="}, required, home)
				if settings.Effort != "" || !strings.Contains(settings.EffortWarning, "does not support") || (problem != nil) != (required != "") {
					t.Fatalf("required=%q settings=%+v problem=%v", required, settings, problem)
				}
			}
		})
	}
}

func TestResolveZCodeEffortMissingLevel(t *testing.T) {
	for _, selection := range []string{
		`{"providerId":"zai","modelId":"glm-5.3"}`,
		`{"providerId":"zai","modelId":"glm-5.3","options":{}}`,
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("ZCODE_PERSONAL_PROVIDER_CONFIG_FILE", "")
		path := filepath.Join(home, ".zcode/cli/provider_config.json")
		writeZCodeEffort(t, path, "high")
		body := `{"schemaVersion":1,"config":{"defaultModelSelection":` + selection + `,"modelConfigRules":{"providerModelRules":[],"manualProviderModelRules":[]}}}`
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"", "low", "high", "max"} {
			settings := model.DelegationSettings{Effort: "high"}
			problem := resolveZCodeEffort(&settings, []string{"ZCODE_MODEL="}, required, home)
			if settings.Effort != "" || !strings.Contains(settings.EffortWarning, "options.reasoningLevel is missing") || (problem != nil) != (required != "") {
				t.Fatalf("required=%q settings=%+v problem=%v", required, settings, problem)
			}
			if problem != nil {
				out, err := json.Marshal(problem)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(out), "options.reasoningLevel is missing") || !strings.Contains(string(out), "zcode-cli-provision") {
					t.Fatalf("missing actionable diagnostic: %s", out)
				}
			}
		}
	}
}

func TestZCodeEffortPlanAndRecord(t *testing.T) {
	for _, level := range []string{"high", "max", ""} {
		t.Run("level_"+level, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("ZCODE_PERSONAL_PROVIDER_CONFIG_FILE", "")
			provider := filepath.Join(home, ".zcode/cli/provider_config.json")
			writeZCodeEffort(t, provider, level)
			f := newDelegateFixture(t, delegateSuccessScript)
			native := filepath.Join(f.root, "zcode")
			if err := os.WriteFile(native, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 0.16.9; exit 0; fi\nprintf '%s\\n' '{\"sessionId\":\"sess_effort\",\"turnId\":\"turn_effort\",\"response\":\"verified answer\"}'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg := map[string]any{"schema_version": 1, "default_profile": "test", "profiles": map[string]any{"test": map[string]any{
				"adapters":          map[string]any{"zcode": map[string]any{"executable": native}},
				"agent_preferences": map[string]any{"mode": "advisory", "preferred": []any{map[string]any{"agent": "zcode", "family": "glm", "model": "zai/glm-5.3"}}},
			}}}
			data, _ := json.Marshal(cfg)
			if err := os.WriteFile(f.config, data, 0600); err != nil {
				t.Fatal(err)
			}
			if level != "high" {
				// Explicit settings constrain the reviewed catalog as well as
				// the runtime profile. Give the catalog the requested tuple.
				requiredData := strings.Replace(string(data), `"model":"zai/glm-5.3"`, `"model":"zai/glm-5.3","effort":"high"`, 1)
				if err := os.WriteFile(f.config, []byte(requiredData), 0600); err != nil {
					t.Fatal(err)
				}
				f.selectRequest(t, `{"family":"glm","settings":{"effort":"high"}}`)
				code, out := f.invoke("reply", "--wait")
				if code != 5 || !strings.Contains(out, "zcode_effort_mismatch") || strings.Contains(out, "sentinel-credential") {
					t.Fatalf("mismatch: exit=%d %s", code, out)
				}
				if _, err := os.Stat(f.journal); !os.IsNotExist(err) {
					t.Fatal("mismatch must refuse before admission")
				}
				if err := os.WriteFile(f.config, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			f.selectRequest(t, `{"family":"glm"}`)
			executionID := ""
			for _, args := range [][]string{{"--plan"}, {"--wait"}} {
				code, out := f.invoke("reply", args...)
				if code != 0 {
					t.Fatalf("exit=%d %s", code, out)
				}
				if level != "" && !strings.Contains(out, `"effort":"`+level+`"`) {
					t.Fatalf("missing effort: %s", out)
				}
				if level == "" && (!strings.Contains(out, `"effort_warning":`) || strings.Contains(out, `"effort":"high"`)) {
					t.Fatalf("unknown effort: %s", out)
				}
				if strings.Contains(out, "sentinel-credential") {
					t.Fatal("output leaked credentials")
				}
				if args[0] == "--wait" {
					executionID = executionIDFromOutput(t, out)
				}
			}
			journal, err := os.ReadFile(f.journal)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(journal), "sentinel-credential") {
				t.Fatal("journal leaked credentials")
			}
			// A replay retains the old observation rather than reading today's config.
			writeZCodeEffort(t, provider, "low")
			code, out := f.invoke("reply", "--plan")
			if code != 0 || strings.Contains(out, `"effort":"low"`) {
				t.Fatalf("replay changed observation: %s", out)
			}
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			a.stdin = strings.NewReader("follow up")
			code = a.run(context.Background(), []string{"--config", f.config, "--journal", f.journal, "continue", executionID, "--request-key", "next-turn", "--mode", "carry_forward", "--prompt-stdin", "--plan"})
			if code != 0 || !strings.Contains(stdout.String(), `"effort":"low"`) {
				t.Fatalf("follow-up must refresh effort: exit=%d %s", code, stdout.String())
			}
		})
	}
}
