// Package zcodeconfig inspects only reviewed, non-secret ZCode launch settings.
package zcodeconfig

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// ProviderPath mirrors the host-managed launcher. A named model takes
// precedence over a personal-file override; an empty selector uses the base.
func ProviderPath(home, selector, override string) string {
	if selector != "" {
		if selector != "glm-5.3" && selector != "glm-5.3-flash" {
			return ""
		}
		return filepath.Join(home, ".zcode", "cli", "models", selector, "provider_config.json")
	}
	if override != "" {
		return override
	}
	return filepath.Join(home, ".zcode", "cli", "provider_config.json")
}

// BuiltinPath mirrors the host launcher's bundled metadata selection.
func BuiltinPath(app, override string) string {
	if override != "" {
		return override
	}
	if app == "" {
		app = "/Applications/ZCode.app"
	}
	return filepath.Join(app, "Contents", "Resources", "config", "provider", "zcode-builtin.json")
}

type effortRuleConfig struct {
	OptionSpecs json.RawMessage `json:"optionSpecs"`
}

type effortExactRule struct {
	ProviderID string           `json:"providerId"`
	ModelID    string           `json:"modelId"`
	Config     effortRuleConfig `json:"config"`
}

// Effort reads the selected model ID and options.reasoningLevel from
// config.defaultModelSelection and validates against the launcher's metadata.
// Unrelated fields (including credentials) are discarded by the decoder.
// Never return decoder or OS errors: they can contain file content or paths.
func Effort(path, builtinPath string) (level, warning string) {
	const unknown = "ZCode profile options.reasoningLevel is unavailable; effort is unknown and the runtime default applies; run zcode-cli-provision to record an explicit supported level"
	const unknownLevels = "ZCode selected model optionSpecs.reasoningLevel.values is unavailable or invalid; effort is unknown"
	var value struct {
		Config struct {
			DefaultModelSelection struct {
				ProviderID string `json:"providerId"`
				ModelID    string `json:"modelId"`
				Options    struct {
					ReasoningLevel string `json:"reasoningLevel"`
				} `json:"options"`
			} `json:"defaultModelSelection"`
			ModelConfigRules struct {
				ProviderModelRules       []effortExactRule `json:"providerModelRules"`
				ManualProviderModelRules []effortExactRule `json:"manualProviderModelRules"`
			} `json:"modelConfigRules"`
		} `json:"config"`
	}
	if !readConfig(path, &value) {
		return "", unknown
	}
	selection := value.Config.DefaultModelSelection
	if selection.Options.ReasoningLevel == "" {
		return "", "ZCode profile options.reasoningLevel is missing; effort is unknown and the runtime default applies; run zcode-cli-provision to record an explicit supported level"
	}
	if selection.ModelID == "" {
		return "", "ZCode profile modelId is missing; effort is unknown"
	}
	// These are the model IDs supported by the reviewed host launcher. Their
	// supported effort levels come exclusively from metadata, never this list.
	switch strings.ToLower(selection.ModelID) {
	case "glm-5.3", "glm-5.3-flash":
	default:
		return "", "ZCode selected model has unknown supported reasoningLevel values; effort is unknown"
	}
	var builtin struct {
		Config struct {
			ModelConfigRules struct {
				ModelRules []struct {
					ModelMatch string           `json:"modelMatch"`
					Config     effortRuleConfig `json:"config"`
				} `json:"modelRules"`
			} `json:"modelConfigRules"`
		} `json:"config"`
	}
	if !readConfig(builtinPath, &builtin) {
		return "", unknownLevels
	}
	var reasoning json.RawMessage
	matched := false
	for _, rule := range builtin.Config.ModelConfigRules.ModelRules {
		// The vendor anchors the entire pattern and matches model IDs without
		// case sensitivity. Unsupported regex syntax is conservatively unknown.
		pattern, err := regexp.Compile("(?i)^(?:" + rule.ModelMatch + ")$")
		if err != nil || rule.ModelMatch == "" {
			return "", unknownLevels
		}
		if pattern.MatchString(selection.ModelID) {
			matched = true
			if !overlayEffortLevels(&reasoning, rule.Config) {
				return "", unknownLevels
			}
		}
	}
	if !matched {
		return "", unknownLevels
	}
	// Personal exact rules overlay matching bundled model rules, in vendor
	// order. An invalid explicit override must not fall back to bundled levels.
	for _, rules := range [][]effortExactRule{value.Config.ModelConfigRules.ProviderModelRules, value.Config.ModelConfigRules.ManualProviderModelRules} {
		for _, rule := range rules {
			if rule.ProviderID == selection.ProviderID && rule.ModelID == selection.ModelID {
				if !overlayEffortLevels(&reasoning, rule.Config) {
					return "", unknownLevels
				}
			}
		}
	}
	levels, valid := effortLevels(reasoning)
	if !valid {
		return "", unknownLevels
	}
	for _, supported := range levels {
		if selection.Options.ReasoningLevel == supported {
			return supported, ""
		}
	}
	return "", "ZCode selected model does not support the profile reasoningLevel; effort is unknown"
}

func readConfig(path string, value any) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return false
	}
	d := json.NewDecoder(io.LimitReader(f, (4<<20)+1))
	return d.Decode(value) == nil && d.Decode(new(struct{})) == io.EOF
}

func overlayEffortLevels(reasoning *json.RawMessage, config effortRuleConfig) bool {
	if config.OptionSpecs == nil {
		return true
	}
	var specs map[string]json.RawMessage
	if json.Unmarshal(config.OptionSpecs, &specs) != nil || specs == nil {
		return false
	}
	value, present := specs["reasoningLevel"]
	if !present {
		return true
	}
	*reasoning = value
	return true
}

func effortLevels(reasoning json.RawMessage) ([]string, bool) {
	var spec struct {
		Values []string `json:"values"`
	}
	if json.Unmarshal(reasoning, &spec) != nil || len(spec.Values) == 0 {
		return nil, false
	}
	seen := make(map[string]bool)
	for _, value := range spec.Values {
		if strings.TrimSpace(value) == "" || len(value) > 256 || strings.ContainsFunc(value, unicode.IsControl) || seen[value] {
			return nil, false
		}
		seen[value] = true
	}
	return spec.Values, true
}
