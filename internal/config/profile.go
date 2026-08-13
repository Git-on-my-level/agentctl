package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ProfileInput is the explicit input accepted by BuildProfile. No field is
// populated from a user's shell, ~/.multica, a Multica cache, or an auth
// store. Supplying a Multica section means all five Multica authority fields
// must be supplied exactly.
type ProfileInput struct {
	// Adapters may be used when a profile contains more than one native adapter.
	// Every Adapter must have a non-empty, exact Executable value.
	Adapters map[string]Adapter

	// AdapterName and AdapterExecutable are the convenient single-adapter form.
	AdapterName       string
	AdapterExecutable string

	// Multica may be supplied as a complete value. The scalar fields are a
	// convenient form for callers constructing input from flags or JSON.
	Multica           *Multica
	MulticaExecutable string
	MulticaProfile    string
	WorkspaceID       string
	ServerURL         string
	AppURL            string
}

// BuildProfile constructs one profile from explicit input. It rejects partial
// adapter or Multica authority rather than silently selecting a default
// executable/profile/workspace or copying credentials from another store.
func BuildProfile(input ProfileInput) (Profile, error) {
	profile := Profile{}

	if len(input.Adapters) > 0 {
		profile.Adapters = cloneAdapters(input.Adapters)
	}
	if strings.TrimSpace(input.AdapterName) != "" || strings.TrimSpace(input.AdapterExecutable) != "" {
		name := strings.TrimSpace(input.AdapterName)
		if name == "" {
			return Profile{}, errors.New("adapter name is required when adapter executable is supplied")
		}
		if strings.TrimSpace(input.AdapterExecutable) == "" {
			return Profile{}, fmt.Errorf("adapter %q executable is required", name)
		}
		if profile.Adapters == nil {
			profile.Adapters = make(map[string]Adapter)
		}
		if _, exists := profile.Adapters[name]; exists {
			return Profile{}, fmt.Errorf("adapter %q supplied more than once", name)
		}
		profile.Adapters[name] = Adapter{Executable: input.AdapterExecutable}
	}

	if input.Multica != nil {
		if anyMulticaScalar(input) {
			return Profile{}, errors.New("multica must be supplied either as a complete value or as scalar fields")
		}
		m := *input.Multica
		if err := m.Validate(); err != nil {
			return Profile{}, fmt.Errorf("multica: %w", err)
		}
		profile.Multica = &m
	} else if anyMulticaScalar(input) {
		m := Multica{
			Executable:  input.MulticaExecutable,
			Profile:     input.MulticaProfile,
			WorkspaceID: input.WorkspaceID,
			ServerURL:   input.ServerURL,
			AppURL:      input.AppURL,
		}
		if err := m.Validate(); err != nil {
			return Profile{}, fmt.Errorf("multica: %w", err)
		}
		profile.Multica = &m
	}

	if len(profile.Adapters) == 0 && profile.Multica == nil {
		return Profile{}, errors.New("profile requires at least one explicit adapter or complete multica authority")
	}
	if err := validateProfile(profile); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

// MergeProfile adds an explicitly named profile to cfg. It is idempotent when
// the profile is already identical, and returns ErrConflict for a different
// profile under the same name. It never chooses a default profile implicitly;
// callers must set Config.DefaultProfile explicitly and validation will check
// that reference.
func MergeProfile(cfg Config, name string, profile Profile) (Config, error) {
	return mergeProfile(cfg, name, profile, false)
}

// UpsertProfile is the explicit replacement form of MergeProfile. It is useful
// for a caller that has already obtained authority to replace an existing
// profile, while preserving all other profiles and default selection.
func UpsertProfile(cfg Config, name string, profile Profile, overwrite bool) (Config, error) {
	return mergeProfile(cfg, name, profile, overwrite)
}

// MergeProfile is also available as a method for callers that prefer fluent
// config construction.
func (c Config) MergeProfile(name string, profile Profile) (Config, error) {
	return MergeProfile(c, name, profile)
}

func mergeProfile(cfg Config, name string, profile Profile, overwrite bool) (Config, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Config{}, errors.New("profile name is required")
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = SchemaVersion
	}
	if cfg.SchemaVersion != SchemaVersion {
		return Config{}, fmt.Errorf("unsupported config schema_version %d", cfg.SchemaVersion)
	}
	if err := validateProfile(profile); err != nil {
		return Config{}, fmt.Errorf("profile %q: %w", name, err)
	}

	result := cfg
	result.Profiles = cloneProfiles(cfg.Profiles)
	if existing, ok := result.Profiles[name]; ok {
		if reflect.DeepEqual(existing, profile) {
			return result, nil
		}
		if !overwrite {
			return Config{}, fmt.Errorf("%w: profile %q already exists", ErrConflict, name)
		}
	}
	result.Profiles[name] = cloneProfile(profile)
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func validateProfile(profile Profile) error {
	cfg := Config{SchemaVersion: SchemaVersion, Profiles: map[string]Profile{"profile": profile}}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return nil
}

func anyMulticaScalar(input ProfileInput) bool {
	return strings.TrimSpace(input.MulticaExecutable) != "" ||
		strings.TrimSpace(input.MulticaProfile) != "" ||
		strings.TrimSpace(input.WorkspaceID) != "" ||
		strings.TrimSpace(input.ServerURL) != "" ||
		strings.TrimSpace(input.AppURL) != ""
}

func cloneAdapters(in map[string]Adapter) map[string]Adapter {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Adapter, len(in))
	for name, adapter := range in {
		out[name] = adapter
	}
	return out
}

func cloneProfiles(in map[string]Profile) map[string]Profile {
	if len(in) == 0 {
		return make(map[string]Profile)
	}
	out := make(map[string]Profile, len(in))
	for name, profile := range in {
		out[name] = cloneProfile(profile)
	}
	return out
}

func cloneProfile(in Profile) Profile {
	out := in
	out.Adapters = cloneAdapters(in.Adapters)
	if in.Multica != nil {
		m := *in.Multica
		out.Multica = &m
	}
	if in.AgentPreferences != nil {
		preferences := *in.AgentPreferences
		preferences.Preferred = append([]AgentPreference(nil), in.AgentPreferences.Preferred...)
		preferences.Notes = append([]string(nil), in.AgentPreferences.Notes...)
		out.AgentPreferences = &preferences
	}
	return out
}
