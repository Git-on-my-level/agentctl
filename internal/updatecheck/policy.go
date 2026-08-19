package updatecheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Mode string

const (
	ModeAuto   Mode = "auto"
	ModeNotify Mode = "notify"
	ModeOff    Mode = "off"
)

type policyDocument struct {
	SchemaVersion int  `json:"schema_version"`
	Mode          Mode `json:"mode"`
}

// DefaultPaths returns binary-global update paths. They deliberately do not
// follow --journal because one installed executable must make one daily check.
func DefaultPaths(getenv func(string) string) (statePath, policyPath string, err error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	root := strings.TrimSpace(getenv("XDG_STATE_HOME"))
	if root == "" {
		home := strings.TrimSpace(getenv("HOME"))
		if home == "" {
			return "", "", errors.New("HOME is required when XDG_STATE_HOME is unset")
		}
		root = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", "", errors.New("update state root must be an absolute clean path")
	}
	root = filepath.Join(root, "agentctl")
	return filepath.Join(root, "update-state.json"), filepath.Join(root, "update-policy.json"), nil
}

func ResolveMode(policyPath string, getenv func(string) string) (Mode, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if raw := strings.TrimSpace(getenv("AGENTCTL_UPDATE_MODE")); raw != "" {
		return parseMode(raw)
	}
	if updateCheckDisabled(getenv) {
		return ModeOff, nil
	}
	if strings.TrimSpace(policyPath) == "" {
		return ModeAuto, nil
	}
	file, err := os.Open(policyPath)
	if errors.Is(err, os.ErrNotExist) {
		return ModeAuto, nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("update policy is not a private regular file")
	}
	var document policyDocument
	if err := json.NewDecoder(file).Decode(&document); err != nil {
		return "", err
	}
	if document.SchemaVersion != stateSchema {
		return "", errors.New("unsupported update policy schema")
	}
	return parseMode(string(document.Mode))
}

func SetMode(policyPath string, mode Mode) error {
	parsed, err := parseMode(string(mode))
	if err != nil {
		return err
	}
	if err := ensurePrivateRoot(filepath.Dir(policyPath)); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(policyPath), ".update-policy.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := json.NewEncoder(temporary).Encode(policyDocument{SchemaVersion: stateSchema, Mode: parsed}); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, policyPath)
}

func parseMode(value string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(value))) {
	case ModeAuto:
		return ModeAuto, nil
	case ModeNotify:
		return ModeNotify, nil
	case ModeOff:
		return ModeOff, nil
	default:
		return "", fmt.Errorf("update mode must be auto, notify, or off")
	}
}

// Due is a read-only fast path used before starting a detached worker.
func Due(options Options) bool {
	_, valid := parseVersion(options.CurrentVersion)
	if !valid || strings.TrimSpace(options.StatePath) == "" {
		return false
	}
	now := time.Now().UTC()
	if options.Now != nil {
		now = options.Now().UTC()
	}
	state, err := readState(options.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	if state.CheckedOn == now.Format("2006-01-02") {
		return false
	}
	if !state.LastAttemptAt.IsZero() && now.Sub(state.LastAttemptAt) < retryInterval {
		return false
	}
	return true
}

type Status struct {
	Mode             Mode   `json:"mode"`
	StatePath        string `json:"state_path"`
	PolicyPath       string `json:"policy_path"`
	CheckedOn        string `json:"checked_on,omitempty"`
	LatestVersion    string `json:"latest_version,omitempty"`
	InstalledVersion string `json:"installed_version,omitempty"`
	InstalledAt      string `json:"installed_at,omitempty"`
	LastErrorCode    string `json:"last_error_code,omitempty"`
	LastErrorAt      string `json:"last_error_at,omitempty"`
}

func ReadStatus(statePath, policyPath string, getenv func(string) string) (Status, error) {
	mode, err := ResolveMode(policyPath, getenv)
	if err != nil {
		return Status{}, err
	}
	result := Status{Mode: mode, StatePath: statePath, PolicyPath: policyPath}
	state, err := readState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return Status{}, err
	}
	result.CheckedOn = state.CheckedOn
	result.LatestVersion = state.LatestVersion
	result.InstalledVersion = state.InstalledVersion
	if !state.InstalledAt.IsZero() {
		result.InstalledAt = state.InstalledAt.Format(time.RFC3339)
	}
	result.LastErrorCode = state.LastErrorCode
	if !state.LastErrorAt.IsZero() {
		result.LastErrorAt = state.LastErrorAt.Format(time.RFC3339)
	}
	return result, nil
}
