// Package updatecheck performs a bounded, fail-open check for newer agentctl
// releases. Its cache is deliberately separate from the execution journal so
// update discovery cannot interfere with execution ownership or recovery.
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultEndpoint = "https://api.github.com/repos/Git-on-my-level/agentctl/releases/latest"
	stateSchema     = 1
	maxResponseSize = 64 << 10
	retryInterval   = time.Hour
	staleLockAge    = 2 * time.Minute
)

type Notice struct {
	CurrentVersion string
	LatestVersion  string
	ReleaseURL     string
}

type Options struct {
	CurrentVersion string
	StatePath      string
	Endpoint       string
	Force          bool
	Now            func() time.Time
	Client         *http.Client
	Getenv         func(string) string
}

type cacheState struct {
	SchemaVersion    int       `json:"schema_version"`
	LastAttemptAt    time.Time `json:"last_attempt_at,omitempty"`
	CheckedOn        string    `json:"checked_on,omitempty"`
	LatestVersion    string    `json:"latest_version,omitempty"`
	ReleaseURL       string    `json:"release_url,omitempty"`
	NotifiedOn       string    `json:"notified_on,omitempty"`
	InstalledVersion string    `json:"installed_version,omitempty"`
	InstalledAt      time.Time `json:"installed_at,omitempty"`
	LastErrorCode    string    `json:"last_error_code,omitempty"`
	LastErrorAt      time.Time `json:"last_error_at,omitempty"`
}

type releaseDocument struct {
	TagName string `json:"tag_name"`
}

// Check returns at most one notice per UTC day. Operational failures are
// returned for tests and diagnostics, but callers should fail open.
func Check(ctx context.Context, options Options) (*Notice, error) {
	current, ok := parseVersion(options.CurrentVersion)
	if !ok || strings.TrimSpace(options.StatePath) == "" || updateCheckDisabled(options.Getenv) {
		return nil, nil
	}
	if err := ensurePrivateRoot(filepath.Dir(options.StatePath)); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if options.Now != nil {
		now = options.Now().UTC()
	}
	today := now.Format("2006-01-02")
	state, stateErr := readState(options.StatePath)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return nil, stateErr
	}
	if state.CheckedOn == today && !options.Force {
		return cachedNotice(options.StatePath, state, current, today)
	}
	if !options.Force && !state.LastAttemptAt.IsZero() && now.Sub(state.LastAttemptAt) < retryInterval {
		return cachedNotice(options.StatePath, state, current, today)
	}

	release, acquired, err := acquireLock(options.StatePath+".lock", now)
	if err != nil || !acquired {
		return nil, err
	}
	defer release()

	// Another process may have completed the daily check before this process
	// acquired the lock.
	state, stateErr = readState(options.StatePath)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return nil, stateErr
	}
	if state.CheckedOn == today && !options.Force {
		return cachedNotice(options.StatePath, state, current, today)
	}
	if !options.Force && !state.LastAttemptAt.IsZero() && now.Sub(state.LastAttemptAt) < retryInterval {
		return cachedNotice(options.StatePath, state, current, today)
	}

	state.SchemaVersion = stateSchema
	state.LastAttemptAt = now
	latest, fetchErr := fetchLatest(ctx, options)
	if fetchErr != nil {
		writeErr := writeState(options.StatePath, state)
		notice, noticeErr := cachedNotice(options.StatePath, state, current, today)
		return notice, errors.Join(fetchErr, writeErr, noticeErr)
	}
	state.CheckedOn = today
	state.LatestVersion = latest
	state.ReleaseURL = releaseURL(latest)
	if parsedLatest, valid := parseVersion(latest); valid && parsedLatest.greaterThan(current) {
		if state.NotifiedOn == today && !options.Force {
			return nil, writeState(options.StatePath, state)
		}
		state.NotifiedOn = today
		notice := &Notice{CurrentVersion: options.CurrentVersion, LatestVersion: latest, ReleaseURL: state.ReleaseURL}
		return notice, writeState(options.StatePath, state)
	}
	state.NotifiedOn = ""
	return nil, writeState(options.StatePath, state)
}

func cachedNotice(path string, state cacheState, current semanticVersion, today string) (*Notice, error) {
	latest, valid := parseVersion(state.LatestVersion)
	if !valid || !latest.greaterThan(current) || state.NotifiedOn == today {
		return nil, nil
	}
	state.NotifiedOn = today
	return &Notice{CurrentVersion: current.String(), LatestVersion: state.LatestVersion, ReleaseURL: state.ReleaseURL}, writeState(path, state)
}

func fetchLatest(ctx context.Context, options Options) (string, error) {
	endpoint := options.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "agentctl/"+options.CurrentVersion)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release check returned HTTP %d", resp.StatusCode)
	}
	var document releaseDocument
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize))
	if err := decoder.Decode(&document); err != nil {
		return "", err
	}
	if _, valid := parseVersion(document.TagName); !valid {
		return "", errors.New("release check returned an invalid version")
	}
	return document.TagName, nil
}

func updateCheckDisabled(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	switch strings.ToLower(strings.TrimSpace(getenv("AGENTCTL_UPDATE_CHECK"))) {
	case "off", "0", "false", "disabled":
		return true
	default:
		return false
	}
}

func readState(path string) (cacheState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return cacheState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return cacheState{}, errors.New("update cache is not a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return cacheState{}, err
	}
	defer file.Close()
	var state cacheState
	if err := json.NewDecoder(io.LimitReader(file, 16<<10)).Decode(&state); err != nil {
		return cacheState{}, err
	}
	if state.SchemaVersion != stateSchema {
		return cacheState{}, errors.New("unsupported update cache schema")
	}
	return state, nil
}

func writeState(path string, state cacheState) error {
	root := filepath.Dir(path)
	if err := ensurePrivateRoot(root); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(root, ".update-check.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := json.NewEncoder(temporary).Encode(state); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func acquireLock(path string, now time.Time) (func(), bool, error) {
	root := filepath.Dir(path)
	if err := ensurePrivateRoot(root); err != nil {
		return func() {}, false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, _ = fmt.Fprintln(file, os.Getpid())
		_ = file.Close()
		return func() { _ = os.Remove(path) }, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return func() {}, false, err
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) <= staleLockAge {
		return func() {}, false, nil
	}
	if removeErr := os.Remove(path); removeErr != nil {
		return func() {}, false, removeErr
	}
	return acquireLock(path, now)
}

func ensurePrivateRoot(path string) error {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	anchor := volume + string(os.PathSeparator)
	relative, err := filepath.Rel(anchor, absolute)
	if err != nil {
		return err
	}
	current := anchor
	for _, part := range strings.Split(relative, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("update cache path contains a symlink")
		}
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("update cache directory is not a private regular directory")
	}
	return nil
}

func releaseURL(version string) string {
	return "https://github.com/Git-on-my-level/agentctl/releases/tag/" + version
}

type semanticVersion [3]uint64

func parseVersion(value string) (semanticVersion, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semanticVersion{}, false
	}
	var parsed semanticVersion
	for index, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return semanticVersion{}, false
		}
		number, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return semanticVersion{}, false
		}
		parsed[index] = number
	}
	return parsed, true
}

func (version semanticVersion) greaterThan(other semanticVersion) bool {
	for index := range version {
		if version[index] != other[index] {
			return version[index] > other[index]
		}
	}
	return false
}

func (version semanticVersion) String() string {
	return fmt.Sprintf("v%d.%d.%d", version[0], version[1], version[2])
}
