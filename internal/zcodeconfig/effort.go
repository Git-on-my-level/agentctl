// Package zcodeconfig inspects only reviewed, non-secret ZCode launch settings.
package zcodeconfig

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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

// Effort reads only config.defaultModelSelection.options.reasoningLevel.
// Unrelated fields (including credentials) are discarded by the decoder.
// Never return decoder or OS errors: they can contain file content or paths.
func Effort(path string) (level, warning string) {
	const unknown = "ZCode profile reasoningLevel is unavailable; effort is unknown and the runtime default applies"
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
	// Do not echo arbitrary config text even from the non-secret field.
	switch level := value.Config.DefaultModelSelection.Options.ReasoningLevel; level {
	case "low", "medium", "high", "xhigh", "max", "ultra":
		return level, ""
	default:
		return "", unknown
	}
}
