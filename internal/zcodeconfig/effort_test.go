package zcodeconfig

import (
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
			got, warning := Effort(path)
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
		if got, warning := Effort(path); got != "" || !strings.Contains(warning, "runtime default") {
			t.Fatalf("unreadable: %q %q", got, warning)
		}
	}
	if os.Geteuid() != 0 {
		path := filepath.Join(t.TempDir(), "unreadable.json")
		if err := os.WriteFile(path, []byte(`{"config":{"defaultModelSelection":{"options":{"reasoningLevel":"high"}}}}`), 0000); err != nil {
			t.Fatal(err)
		}
		if got, warning := Effort(path); got != "" || warning == "" {
			t.Fatalf("permission denied: %q %q", got, warning)
		}
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
