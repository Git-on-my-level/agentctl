package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// BundleSchemaVersion is versioned independently from the live config file.
// A bundle is an explicit, local, declarative overlay; it is never discovered
// or fetched by agentctl.
const BundleSchemaVersion = 1

const BundleMaxBytes int64 = 1 << 20

// Bundle is intentionally narrower than Config. In particular, it cannot
// supply adapter arguments, callback commands, install roots, or credentials.
type Bundle struct {
	SchemaVersion  int                      `json:"schema_version"`
	DefaultProfile string                   `json:"default_profile,omitempty"`
	Profiles       map[string]BundleProfile `json:"profiles"`
}

type BundleProfile struct {
	Adapters map[string]BundleAdapter `json:"adapters,omitempty"`
	Multica  *Multica                 `json:"multica,omitempty"`
}

type BundleAdapter struct {
	Executable string `json:"executable"`
}

type BundleProvenance struct {
	SourcePath    string `json:"source_path"`
	SHA256        string `json:"sha256"`
	SchemaVersion int    `json:"schema_version"`
	Bytes         int64  `json:"bytes"`
}

// Resolution makes precedence and provenance observable without copying them
// into execution records or event payloads.
type Resolution struct {
	Config      Config            `json:"config"`
	BasePath    string            `json:"base_path"`
	BasePresent bool              `json:"base_present"`
	Bundle      *BundleProvenance `json:"bundle,omitempty"`
	Composition []string          `json:"composition_order"`
	Notes       []string          `json:"notes,omitempty"`
}

type BundlePlan struct {
	Valid          bool             `json:"valid"`
	Provenance     BundleProvenance `json:"provenance"`
	AddProfiles    []string         `json:"add_profiles,omitempty"`
	Existing       []string         `json:"existing_profiles,omitempty"`
	DefaultProfile string           `json:"default_profile,omitempty"`
	Mutates        bool             `json:"mutates"`
	RuntimeScope   []string         `json:"runtime_scope"`
}

// LoadBundle reads one explicitly named local file. Ordinary 0644 files from
// a Git checkout are accepted, but symlinks and group/world-writable sources
// fail closed. It performs no discovery, network, environment, or Git work.
func LoadBundle(path string) (Bundle, BundleProvenance, error) {
	clean, err := cleanSourcePath(path)
	if err != nil {
		return Bundle{}, BundleProvenance{}, err
	}
	info, err := os.Lstat(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Bundle{}, BundleProvenance{}, ErrNotFound
		}
		return Bundle{}, BundleProvenance{}, fmt.Errorf("inspect config bundle: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Bundle{}, BundleProvenance{}, fmt.Errorf("%w: bundle must be a regular non-symlink file", ErrUnmanaged)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return Bundle{}, BundleProvenance{}, fmt.Errorf("%w: bundle must not be group or world writable", ErrUnmanaged)
	}
	if info.Size() > BundleMaxBytes {
		return Bundle{}, BundleProvenance{}, fmt.Errorf("config bundle exceeds %d-byte limit", BundleMaxBytes)
	}
	data, err := os.ReadFile(clean)
	if err != nil {
		return Bundle{}, BundleProvenance{}, fmt.Errorf("read config bundle: %w", err)
	}
	bundle, err := decodeBundle(data)
	if err != nil {
		return Bundle{}, BundleProvenance{}, err
	}
	digest := sha256.Sum256(data)
	return bundle, BundleProvenance{SourcePath: clean, SHA256: hex.EncodeToString(digest[:]), SchemaVersion: bundle.SchemaVersion, Bytes: int64(len(data))}, nil
}

// Resolve applies a bundle additively after the base config. Existing profile
// definitions and an existing default cannot be replaced.
func Resolve(basePath, bundlePath string) (Resolution, error) {
	result := Resolution{BasePath: basePath, Composition: []string{"safe_builtins", "user_config", "explicit_additive_bundle"}}
	base, err := Load(basePath)
	if err == nil {
		result.BasePresent = true
	} else if errors.Is(err, ErrNotFound) && strings.TrimSpace(bundlePath) != "" {
		base = Config{SchemaVersion: SchemaVersion, Profiles: map[string]Profile{}}
	} else {
		return Resolution{}, err
	}
	if strings.TrimSpace(bundlePath) == "" {
		result.Config = base
		return result, nil
	}
	bundle, provenance, err := LoadBundle(bundlePath)
	if err != nil {
		return Resolution{}, err
	}
	effective, err := applyBundle(base, bundle)
	if err != nil {
		return Resolution{}, err
	}
	result.Config = effective
	result.Bundle = &provenance
	result.Notes = []string{"bundle profiles compose additively and collisions fail closed", "native adapter executable entries are doctor validation expectations only; run argv is unchanged", "bundle provenance is not copied into execution events"}
	return result, nil
}

func PlanBundle(basePath, bundlePath string) (BundlePlan, error) {
	bundle, provenance, err := LoadBundle(bundlePath)
	if err != nil {
		return BundlePlan{}, err
	}
	resolution, err := Resolve(basePath, bundlePath)
	if err != nil {
		return BundlePlan{}, err
	}
	baseProfiles := map[string]Profile{}
	if resolution.BasePresent {
		base, err := Load(basePath)
		if err != nil {
			return BundlePlan{}, err
		}
		baseProfiles = base.Profiles
	}
	add := make([]string, 0, len(bundle.Profiles))
	existing := make([]string, 0, len(bundle.Profiles))
	for name := range bundle.Profiles {
		if _, ok := baseProfiles[name]; ok {
			existing = append(existing, name)
		} else {
			add = append(add, name)
		}
	}
	sort.Strings(add)
	sort.Strings(existing)
	return BundlePlan{Valid: true, Provenance: provenance, AddProfiles: add, Existing: existing, DefaultProfile: bundle.DefaultProfile, Mutates: false, RuntimeScope: []string{"profile resolution", "multica authority", "config doctor provenance"}}, nil
}

func applyBundle(base Config, bundle Bundle) (Config, error) {
	result := base
	result.Profiles = cloneProfiles(base.Profiles)
	names := make([]string, 0, len(bundle.Profiles))
	for name := range bundle.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		profile := bundle.Profiles[name].profile()
		if existing, ok := result.Profiles[name]; ok {
			if !reflect.DeepEqual(existing, profile) {
				return Config{}, fmt.Errorf("%w: bundle profile %q would replace user config", ErrConflict, name)
			}
			continue
		}
		result.Profiles[name] = profile
	}
	if bundle.DefaultProfile != "" {
		if result.DefaultProfile != "" && result.DefaultProfile != bundle.DefaultProfile {
			return Config{}, fmt.Errorf("%w: bundle default_profile would replace user config default", ErrConflict)
		}
		result.DefaultProfile = bundle.DefaultProfile
	}
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func (p BundleProfile) profile() Profile {
	result := Profile{Multica: p.Multica}
	if len(p.Adapters) != 0 {
		result.Adapters = make(map[string]Adapter, len(p.Adapters))
		for name, adapter := range p.Adapters {
			result.Adapters[name] = Adapter{Executable: adapter.Executable}
		}
	}
	return result
}

func decodeBundle(data []byte) (Bundle, error) {
	var bundle Bundle
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, fmt.Errorf("parse config bundle: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Bundle{}, errors.New("parse config bundle: trailing JSON document")
	}
	if err := bundle.Validate(); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func (b Bundle) Validate() error {
	if b.SchemaVersion != BundleSchemaVersion {
		return fmt.Errorf("unsupported config bundle schema_version %d", b.SchemaVersion)
	}
	if len(b.Profiles) == 0 {
		return errors.New("config bundle requires at least one profile")
	}
	config := Config{SchemaVersion: SchemaVersion, DefaultProfile: b.DefaultProfile, Profiles: make(map[string]Profile, len(b.Profiles))}
	for name, profile := range b.Profiles {
		config.Profiles[name] = profile.profile()
	}
	return config.Validate()
}

func cleanSourcePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("config bundle path is required")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve config bundle path: %w", err)
	}
	return abs, nil
}
