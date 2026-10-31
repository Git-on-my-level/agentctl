// Package zcodeconfig inspects only reviewed, non-secret ZCode launch settings.
package zcodeconfig

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// Effort reads the selected model ID and options.reasoningLevel from
// config.defaultModelSelection and validates the model's reviewed levels.
// Unrelated fields (including credentials) are discarded by the decoder.
// Never return decoder or OS errors: they can contain file content or paths.
func Effort(path string) (level, warning string) {
	const unknown = "ZCode profile options.reasoningLevel is unavailable; effort is unknown and the runtime default applies; run zcode-cli-provision to record an explicit supported level"
	f, err := os.Open(path)
	if err != nil {
		return "", unknown
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return "", unknown
	}
	var value struct {
		Config struct {
			DefaultModelSelection struct {
				ModelID string `json:"modelId"`
				Options struct {
					ReasoningLevel string `json:"reasoningLevel"`
				} `json:"options"`
			} `json:"defaultModelSelection"`
		} `json:"config"`
	}
	d := json.NewDecoder(io.LimitReader(f, (4<<20)+1))
	if d.Decode(&value) != nil || d.Decode(new(struct{})) != io.EOF {
		return "", unknown
	}
	selection := value.Config.DefaultModelSelection
	if selection.Options.ReasoningLevel == "" {
		return "", "ZCode profile options.reasoningLevel is missing; effort is unknown and the runtime default applies; run zcode-cli-provision to record an explicit supported level"
	}
	if selection.ModelID == "" {
		return "", "ZCode profile modelId is missing; effort is unknown"
	}
	// These are the reviewed GLM-5.3 model IDs and supported levels. Never
	// assume another selected model accepts the same effort vocabulary.
	switch strings.ToLower(selection.ModelID) {
	case "glm-5.3", "glm-5.3-flash":
	default:
		return "", "ZCode selected model has unknown supported reasoningLevel values; effort is unknown"
	}
	// Do not echo arbitrary config text even from the non-secret fields.
	switch level := selection.Options.ReasoningLevel; level {
	case "low", "high", "max":
		return level, ""
	}
	return "", "ZCode selected model does not support the profile reasoningLevel; effort is unknown"
}
