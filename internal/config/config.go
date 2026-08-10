// Package config loads explicit agentctl profiles without probing or mutating
// external systems. Live capability and authentication checks belong to doctor
// and adapter probes.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const SchemaVersion = 1

var (
	ErrNotFound       = errors.New("agentctl config not found")
	ErrProfileMissing = errors.New("agentctl profile not found")
)

type Config struct {
	SchemaVersion  int                `json:"schema_version"`
	DefaultProfile string             `json:"default_profile,omitempty"`
	Profiles       map[string]Profile `json:"profiles,omitempty"`
}

type Profile struct {
	Adapters map[string]Adapter `json:"adapters,omitempty"`
	Multica  *Multica           `json:"multica,omitempty"`
}

type Adapter struct {
	Executable string   `json:"executable,omitempty"`
	Arguments  []string `json:"arguments,omitempty"`
}

// Multica deliberately carries the exact profile, workspace, and link origin.
// This prevents an unqualified Multica CLI invocation from selecting a stale
// or unauthenticated default profile.
type Multica struct {
	Executable  string `json:"executable"`
	Profile     string `json:"profile"`
	WorkspaceID string `json:"workspace_id"`
	ServerURL   string `json:"server_url"`
	AppURL      string `json:"app_url"`
}

// DefaultPath returns the documented config location without creating it.
func DefaultPath() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("AGENTCTL_CONFIG")); explicit != "" {
		return filepath.Clean(explicit), nil
	}
	if root := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); root != "" {
		return filepath.Join(root, "agentctl", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "agentctl", "config.json"), nil
}

// Load is strictly read-only: it never creates, chmods, refreshes, or repairs.
func Load(path string) (Config, error) {
	clean, err := cleanSavePath(path)
	if err != nil {
		return Config{}, err
	}
	managed, exists, cfg, err := existingConfigState(clean)
	if err != nil {
		return Config{}, err
	}
	if !exists {
		return Config{}, ErrNotFound
	}
	if !managed {
		return Config{}, fmt.Errorf("%w: config must be a regular owner-only 0600 file", ErrUnmanaged)
	}
	// The file itself is safe; now reject any symlinked parent component.
	// Its parent necessarily exists, so ensureOwnerPath performs no writes.
	if err := ensureOwnerPath(filepath.Dir(clean)); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported config schema_version %d", c.SchemaVersion)
	}
	if c.DefaultProfile != "" {
		if _, ok := c.Profiles[c.DefaultProfile]; !ok {
			return fmt.Errorf("default profile %q is not defined", c.DefaultProfile)
		}
	}
	for name, profile := range c.Profiles {
		if strings.TrimSpace(name) == "" {
			return errors.New("profile name cannot be empty")
		}
		for adapterName, adapter := range profile.Adapters {
			if strings.TrimSpace(adapterName) == "" || strings.TrimSpace(adapter.Executable) == "" {
				return fmt.Errorf("profile %q has invalid adapter entry", name)
			}
		}
		if profile.Multica != nil {
			if err := profile.Multica.Validate(); err != nil {
				return fmt.Errorf("profile %q multica: %w", name, err)
			}
		}
	}
	return nil
}

func (m Multica) Validate() error {
	for field, value := range map[string]string{
		"executable": m.Executable, "profile": m.Profile,
		"workspace_id": m.WorkspaceID, "server_url": m.ServerURL,
		"app_url": m.AppURL,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	for field, raw := range map[string]string{"server_url": m.ServerURL, "app_url": m.AppURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("%s must be an absolute URL", field)
		}
		if parsed.User != nil {
			return fmt.Errorf("%s must not contain credentials", field)
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("%s must not contain a query or fragment", field)
		}
	}
	return nil
}

func (c Config) ResolveProfile(name string) (string, Profile, error) {
	resolved := strings.TrimSpace(name)
	if resolved == "" {
		resolved = c.DefaultProfile
	}
	if resolved == "" {
		return "", Profile{}, fmt.Errorf("%w: select --profile or configure default_profile", ErrProfileMissing)
	}
	profile, ok := c.Profiles[resolved]
	if !ok {
		return "", Profile{}, fmt.Errorf("%w: %s", ErrProfileMissing, resolved)
	}
	return resolved, profile, nil
}
