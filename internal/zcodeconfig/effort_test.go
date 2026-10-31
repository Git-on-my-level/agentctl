package zcodeconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffort(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"high", `{"secret":"sentinel-credential","config":{"defaultModelSelection":{"providerId":"zai","modelId":"GLM-5.3","options":{"reasoningLevel":"high"}}}}`, "high"},
		{"max", `{"config":{"defaultModelSelection":{"providerId":"zai","modelId":"glm-5.3-flash","options":{"reasoningLevel":"max"}}}}`, "max"},
		{"low", `{"config":{"defaultModelSelection":{"providerId":"zai","modelId":"glm-5.3","options":{"reasoningLevel":"low"}}}}`, "low"},
		{"medium_unsupported", `{"config":{"defaultModelSelection":{"providerId":"zai","modelId":"glm-5.3","options":{"reasoningLevel":"medium"}}}}`, ""},
		{"xhigh_unsupported", `{"config":{"defaultModelSelection":{"providerId":"zai","modelId":"glm-5.3","options":{"reasoningLevel":"xhigh"}}}}`, ""},
		{"ultra_unsupported", `{"config":{"defaultModelSelection":{"providerId":"zai","modelId":"glm-5.3","options":{"reasoningLevel":"ultra"}}}}`, ""},
		{"different_model", `{"config":{"defaultModelSelection":{"providerId":"zai","modelId":"glm-4.7","options":{"reasoningLevel":"high"}}}}`, ""},
		{"missing_model", `{"config":{"defaultModelSelection":{"options":{"reasoningLevel":"high"}}}}`, ""},
		{"missing", `{"config":{"defaultModelSelection":{"providerId":"zai","modelId":"glm-5.3"}}}`, ""},
		{"missing_options_level", `{"config":{"defaultModelSelection":{"providerId":"zai","modelId":"glm-5.3","options":{}}}}`, ""},
		{"malformed", `{"sentinel-credential":`, ""},
		{"invalid_type", `{"config":{"defaultModelSelection":{"options":{"reasoningLevel":{"sentinel-credential":true}}}}}`, ""},
		{"invalid_level", `{"config":{"defaultModelSelection":{"options":{"reasoningLevel":"sentinel-credential"}}}}`, ""},
		{"trailing_json", `{} {"sentinel-credential":true}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "provider.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			got, warning := Effort(path, "testdata/builtin.json")
			if got != tc.want || (warning != "") != (tc.want == "") {
				t.Fatalf("got %q, %q", got, warning)
			}
			if strings.Contains(got+warning, "sentinel-credential") || strings.Contains(warning, path) {
				t.Fatal("config data leaked")
			}
			if strings.HasPrefix(tc.name, "missing") && tc.name != "missing_model" && (!strings.Contains(warning, "options.reasoningLevel") || !strings.Contains(warning, "zcode-cli-provision")) {
				t.Fatalf("missing field must name the field and provisioner fix: %q", warning)
			}
		})
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		if got, warning := Effort(path, "testdata/builtin.json"); got != "" || !strings.Contains(warning, "runtime default") {
			t.Fatalf("unreadable: %q %q", got, warning)
		}
	}
	if os.Geteuid() != 0 {
		path := filepath.Join(t.TempDir(), "unreadable.json")
		if err := os.WriteFile(path, []byte(`{"config":{"defaultModelSelection":{"options":{"reasoningLevel":"high"}}}}`), 0000); err != nil {
			t.Fatal(err)
		}
		if got, warning := Effort(path, "testdata/builtin.json"); got != "" || warning == "" {
			t.Fatalf("permission denied: %q %q", got, warning)
		}
	}
}

func TestEffortMetadata(t *testing.T) {
	profile, err := os.ReadFile("testdata/profile.json")
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := os.ReadFile("testdata/builtin.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, profile, builtin, want string }{
		{"listed", string(profile), string(builtin), "high"},
		{"flash", strings.ReplaceAll(string(profile), "glm-5.3", "GLM-5.3-FLASH"), string(builtin), "high"},
		{"unlisted", strings.ReplaceAll(string(profile), `"high"`, `"medium"`), string(builtin), ""},
		{"metadata_not_hardcoded", strings.ReplaceAll(string(profile), `"high"`, `"medium"`), strings.ReplaceAll(string(builtin), `"high"`, `"medium"`), "medium"},
		{"absent_list", string(profile), strings.ReplaceAll(string(builtin), `"values"`, `"ignored"`), ""},
		{"empty_list", string(profile), `{"config":{"modelConfigRules":{"modelRules":[{"modelMatch":".*glm-5\\.3.*","config":{"optionSpecs":{"reasoningLevel":{"values":[]}}}}]}}}`, ""},
		{"malformed_list", string(profile), `{"config":{"modelConfigRules":{"modelRules":[{"modelMatch":".*glm-5\\.3.*","config":{"optionSpecs":{"reasoningLevel":{"values":["high",17]}}}}]}}}`, ""},
		{"null_list", string(profile), `{"config":{"modelConfigRules":{"modelRules":[{"modelMatch":".*","config":{"optionSpecs":{"reasoningLevel":{"values":null}}}}]}}}`, ""},
		{"duplicate_list", string(profile), `{"config":{"modelConfigRules":{"modelRules":[{"modelMatch":".*","config":{"optionSpecs":{"reasoningLevel":{"values":["high","high"]}}}}]}}}`, ""},
		{"blank_list_entry", string(profile), `{"config":{"modelConfigRules":{"modelRules":[{"modelMatch":".*","config":{"optionSpecs":{"reasoningLevel":{"values":["high",""]}}}}]}}}`, ""},
		{"no_match", string(profile), strings.ReplaceAll(string(builtin), "glm-5", "glm-4"), ""},
		{"anchored_match", string(profile), strings.ReplaceAll(string(builtin), `.*glm-5\\.3.*`, "glm-5"), ""},
		{"invalid_regex", string(profile), strings.ReplaceAll(string(builtin), `.*glm-5\\.3.*`, "["), ""},
		{"invalid_json", string(profile), `{"sentinel-credential":`, ""},
		{"unreadable", string(profile), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path, metadata := filepath.Join(root, "profile.json"), filepath.Join(root, "builtin.json")
			if err := os.WriteFile(path, []byte(tc.profile), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.builtin != "" {
				if err := os.WriteFile(metadata, []byte(tc.builtin), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, warning := Effort(path, metadata)
			if got != tc.want || (warning != "") != (tc.want == "") {
				t.Fatalf("got %q, %q", got, warning)
			}
			if strings.Contains(warning, root) || strings.Contains(warning, "sentinel-credential") {
				t.Fatal("metadata content leaked")
			}
		})
	}
}

func TestBuiltinPath(t *testing.T) {
	for _, tc := range []struct{ app, override, want string }{
		{"", "", "/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json"},
		{"/custom/ZCode.app", "", "/custom/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json"},
		{"/ignored", "relative.json", "relative.json"},
	} {
		if got := BuiltinPath(tc.app, tc.override); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}

func TestEffortRulePrecedence(t *testing.T) {
	profile, err := os.ReadFile("testdata/profile.json")
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := os.ReadFile("testdata/builtin.json")
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(builtin, &metadata); err != nil {
		t.Fatal(err)
	}
	rules := metadata["config"].(map[string]any)["modelConfigRules"].(map[string]any)
	selected := rules["modelRules"].([]any)[0]
	// Real bundles have broad rules before the GLM rule. Do not validate an
	// intermediate null list before later matching rules supply its values.
	rules["modelRules"] = []any{
		map[string]any{"modelMatch": ".*", "config": map[string]any{"optionSpecs": map[string]any{"reasoningLevel": map[string]any{"values": nil}}}},
		selected,
		map[string]any{"modelMatch": "other-model", "config": map[string]any{"optionSpecs": map[string]any{"reasoningLevel": map[string]any{"values": []string{"medium"}}}}},
	}
	builtin, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, personal, want string }{
		{"matching_rule_not_index", `{"providerModelRules":[],"manualProviderModelRules":[]}`, "high"},
		{"other_provider_ignored", `{"providerModelRules":[{"providerId":"other","modelId":"glm-5.3","config":{"optionSpecs":{"reasoningLevel":{"values":["medium"]}}}}]}`, "high"},
		{"other_model_ignored", `{"providerModelRules":[{"providerId":"zai","modelId":"glm-5.3-flash","config":{"optionSpecs":{"reasoningLevel":{"values":["medium"]}}}}]}`, "high"},
		{"profile_overrides", `{"providerModelRules":[{"providerId":"zai","modelId":"glm-5.3","config":{"optionSpecs":{"reasoningLevel":{"values":["medium"]}}}}]}`, ""},
		{"manual_overrides", `{"manualProviderModelRules":[{"providerId":"zai","modelId":"glm-5.3","config":{"optionSpecs":{"reasoningLevel":{"values":["medium"]}}}}]}`, ""},
		{"profile_null_no_fallback", `{"providerModelRules":[{"providerId":"zai","modelId":"glm-5.3","config":{"optionSpecs":{"reasoningLevel":null}}}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var personal map[string]any
			if err := json.Unmarshal(profile, &personal); err != nil {
				t.Fatal(err)
			}
			var exact json.RawMessage = []byte(tc.personal)
			personal["config"].(map[string]any)["modelConfigRules"] = exact
			body, err := json.Marshal(personal)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			path, bundled := filepath.Join(root, "profile.json"), filepath.Join(root, "builtin.json")
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bundled, builtin, 0600); err != nil {
				t.Fatal(err)
			}
			got, warning := Effort(path, bundled)
			if got != tc.want || (warning != "") != (tc.want == "") {
				t.Fatalf("got %q, %q", got, warning)
			}
		})
	}
}

func TestProviderPath(t *testing.T) {
	for _, tc := range []struct{ selector, override, want string }{
		{"", "", "/home/test/.zcode/cli/provider_config.json"},
		{"", "/override.json", "/override.json"},
		{"glm-5.3-flash", "/ignored.json", "/home/test/.zcode/cli/models/glm-5.3-flash/provider_config.json"},
		{"glm-5.3", "", "/home/test/.zcode/cli/models/glm-5.3/provider_config.json"},
		{"../invalid", "", ""},
	} {
		if got := ProviderPath("/home/test", tc.selector, tc.override); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}
