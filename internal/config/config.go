// Package config loads explicit agentctl profiles without probing or mutating
// external systems. Live capability and authentication checks belong to doctor
// and adapter probes.
package config

import (
	"errors"
	"fmt"
	"net"
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
	Skills         *Skills            `json:"skills,omitempty"`
}

// Skills selects a reviewed pack in a separate Git-backed Skill Hub. The
// config repository owns only this selection and update policy; the Skill Hub
// owns all skill bodies and manifests.
type Skills struct {
	Source       SkillSource `json:"source"`
	UpdatePolicy string      `json:"update_policy"`
}

type SkillSource struct {
	Remote       string `json:"remote"`
	Ref          string `json:"ref,omitempty"`
	ManifestPath string `json:"manifest_path"`
}

type Profile struct {
	Adapters         map[string]Adapter `json:"adapters,omitempty"`
	Multica          *Multica           `json:"multica,omitempty"`
	AgentPreferences *AgentPreferences  `json:"agent_preferences,omitempty"`
	Route            *Route             `json:"route,omitempty"`
}

type Adapter struct {
	Executable string `json:"executable,omitempty"`
}

// AgentPreferences records human- and agent-readable delegation guidance.
// Mode stays advisory: agentctl never rewrites native run argv. Dispatch and
// route explain treat preferred[] as the reviewed adapter+model table and fail
// closed on off-policy selectors. Off-policy run argv warns and still launches.
type AgentPreferences struct {
	Mode      string            `json:"mode"`
	Preferred []AgentPreference `json:"preferred"`
	Notes     []string          `json:"notes,omitempty"`
}

type AgentPreference struct {
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Speed  string `json:"speed"`
	UseFor string `json:"use_for,omitempty"`
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

// Route is optional keyword/placement data for `route explain <query>`.
// It is not required for single-machine use.
type Route struct {
	ThisHost  string            `json:"this_host,omitempty"`
	Hosts     map[string]string `json:"hosts,omitempty"`
	Placement *RoutePlacement   `json:"placement,omitempty"`
}

type RoutePlacement struct {
	Kind string `json:"kind"`
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
	if c.Skills != nil {
		if err := c.Skills.Validate(); err != nil {
			return fmt.Errorf("skills: %w", err)
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
		if profile.AgentPreferences != nil {
			if err := profile.AgentPreferences.Validate(); err != nil {
				return fmt.Errorf("profile %q agent_preferences: %w", name, err)
			}
		}
		if profile.Route != nil {
			if err := profile.Route.Validate(); err != nil {
				return fmt.Errorf("profile %q route: %w", name, err)
			}
		}
	}
	return nil
}

func (s Skills) Validate() error {
	s.Source.Remote = strings.TrimSpace(s.Source.Remote)
	s.Source.Ref = strings.TrimSpace(s.Source.Ref)
	s.Source.ManifestPath = strings.TrimSpace(s.Source.ManifestPath)
	if s.Source.Remote == "" {
		return errors.New("source.remote is required")
	}
	if strings.ContainsAny(s.Source.Remote, "\r\n\x00") {
		return errors.New("source.remote contains forbidden control characters")
	}
	if err := validateSourceRemote(s.Source.Remote); err != nil {
		return fmt.Errorf("source.remote: %w", err)
	}
	if s.Source.Ref == "" {
		s.Source.Ref = "main"
	}
	if strings.ContainsAny(s.Source.Ref, "\r\n\x00 ~^:?*[\\") || strings.HasPrefix(s.Source.Ref, "-") || strings.Contains(s.Source.Ref, "..") || strings.HasSuffix(s.Source.Ref, "/") || strings.HasSuffix(s.Source.Ref, ".") {
		return errors.New("source.ref is not a safe Git ref")
	}
	manifest := filepath.ToSlash(filepath.Clean(s.Source.ManifestPath))
	if s.Source.ManifestPath == "" || filepath.IsAbs(s.Source.ManifestPath) || strings.ContainsAny(s.Source.ManifestPath, "\r\n\x00\\") || manifest == "." || manifest == ".." || strings.HasPrefix(manifest, "../") || manifest == ".git" || strings.HasPrefix(manifest, ".git/") {
		return errors.New("source.manifest_path must be a repository-relative file outside .git")
	}
	switch s.UpdatePolicy {
	case "manual", "auto-clean":
	default:
		return errors.New("update_policy must be manual or auto-clean")
	}
	return nil
}

func (p AgentPreferences) Validate() error {
	if p.Mode != "advisory" {
		return errors.New("mode must be advisory")
	}
	if len(p.Preferred) == 0 {
		return errors.New("preferred requires at least one entry")
	}
	if len(p.Preferred) > 32 {
		return errors.New("preferred exceeds 32-entry limit")
	}
	seen := make(map[string]struct{}, len(p.Preferred))
	for i, preference := range p.Preferred {
		fields := map[string]string{"agent": preference.Agent, "model": preference.Model, "speed": preference.Speed}
		for field, value := range fields {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("preferred[%d].%s is required", i, field)
			}
			if len(value) > 128 {
				return fmt.Errorf("preferred[%d].%s exceeds 128-byte limit", i, field)
			}
		}
		if len(preference.UseFor) > 128 {
			return fmt.Errorf("preferred[%d].use_for exceeds 128-byte limit", i)
		}
		key := preference.Agent + "\x00" + preference.Model + "\x00" + preference.Speed + "\x00" + preference.UseFor
		if _, ok := seen[key]; ok {
			return fmt.Errorf("preferred[%d] duplicates an earlier entry", i)
		}
		seen[key] = struct{}{}
	}
	if len(p.Notes) > 16 {
		return errors.New("notes exceeds 16-entry limit")
	}
	for i, note := range p.Notes {
		if strings.TrimSpace(note) == "" {
			return fmt.Errorf("notes[%d] cannot be empty", i)
		}
		if len(note) > 1024 {
			return fmt.Errorf("notes[%d] exceeds 1024-byte limit", i)
		}
	}
	return nil
}

func (r Route) Validate() error {
	if len(r.ThisHost) > 128 {
		return errors.New("this_host exceeds 128-byte limit")
	}
	if len(r.Hosts) > 64 {
		return errors.New("hosts exceeds 64-entry limit")
	}
	for alias, id := range r.Hosts {
		if strings.TrimSpace(alias) == "" || len(alias) > 128 {
			return errors.New("hosts contains an invalid alias")
		}
		if strings.TrimSpace(id) == "" || len(id) > 128 {
			return fmt.Errorf("hosts[%q] has an invalid id", alias)
		}
	}
	if r.Placement != nil {
		kind := strings.TrimSpace(r.Placement.Kind)
		if kind == "" || len(kind) > 64 {
			return errors.New("placement.kind must be 1-64 characters")
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
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
			return fmt.Errorf("%s must use https (http is allowed only for loopback)", field)
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
