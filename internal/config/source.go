package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const SourceSchemaVersion = 1

const defaultSourceBundle = "config-bundle.json"

var scpRemotePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._/-]+$`)

var (
	ErrSourceGit  = errors.New("agentctl config source Git operation failed")
	ErrSourceAuth = errors.New("agentctl config source Git authentication failed")
)

// SourceSpec is the reviewed, non-secret declaration for a Git-backed config.
// Git and SSH remain solely responsible for credentials and authentication.
type SourceSpec struct {
	Remote       string `json:"remote"`
	Ref          string `json:"ref"`
	BundlePath   string `json:"bundle_path"`
	CheckoutPath string `json:"checkout_path"`
}

// SourceState binds the live owner-only config to the exact Git revision and
// bundle bytes that last produced it.
type SourceState struct {
	SchemaVersion int        `json:"schema_version"`
	Spec          SourceSpec `json:"source"`
	AppliedCommit string     `json:"applied_commit"`
	BundleSHA256  string     `json:"bundle_sha256"`
	LiveSHA256    string     `json:"live_config_sha256"`
	AppliedAt     time.Time  `json:"applied_at"`
}

type SourcePlan struct {
	Operation       string            `json:"operation"`
	Source          SourceSpec        `json:"source"`
	ConfigPath      string            `json:"config_path"`
	StatePath       string            `json:"state_path"`
	PlanEffects     SourceSideEffects `json:"plan_invocation_side_effects"`
	ApplyEffects    SourceSideEffects `json:"apply_invocation_side_effects"`
	RemoteValidated bool              `json:"remote_validated"`
	Safety          []string          `json:"safety"`
}

type SourceSideEffects struct {
	NetworkAccess bool `json:"network_access"`
	Mutates       bool `json:"mutates"`
}

type SourceStatus struct {
	Configured      bool       `json:"configured"`
	InSync          bool       `json:"in_sync"`
	Source          SourceSpec `json:"source,omitempty"`
	StatePath       string     `json:"state_path"`
	ConfigPath      string     `json:"config_path"`
	AppliedCommit   string     `json:"applied_commit,omitempty"`
	CheckoutCommit  string     `json:"checkout_commit,omitempty"`
	BundleSHA256    string     `json:"bundle_sha256,omitempty"`
	CheckoutSHA256  string     `json:"checkout_bundle_sha256,omitempty"`
	LiveSHA256      string     `json:"live_config_sha256,omitempty"`
	ExpectedLiveSHA string     `json:"expected_live_config_sha256,omitempty"`
	CheckoutClean   bool       `json:"checkout_clean"`
	Drift           []string   `json:"drift,omitempty"`
	LastAppliedAt   *time.Time `json:"last_applied_at,omitempty"`
}

type SourceUpdate struct {
	Changed bool         `json:"changed"`
	Status  SourceStatus `json:"status"`
}

// RestoreSource replaces only a drifted live config with the exact validated
// bundle already pinned in the managed checkout. It performs no network work
// and never advances the configured Git revision.
func RestoreSource(ctx context.Context, configPath string) (SourceUpdate, error) {
	return RestoreSourceWithGit(ctx, NativeSourceGit{}, configPath)
}

func RestoreSourceWithGit(ctx context.Context, git SourceGit, configPath string) (SourceUpdate, error) {
	cleanConfig, err := cleanSavePath(configPath)
	if err != nil {
		return SourceUpdate{}, err
	}
	lock, err := acquireSourceLock(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	defer lock.Close()
	statePath, err := SourceStatePath(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	state, err := LoadSourceState(statePath)
	if err != nil {
		return SourceUpdate{}, err
	}
	if err := verifyManagedCheckout(ctx, git, state.Spec); err != nil {
		return SourceUpdate{}, err
	}
	commitBytes, err := git.Run(ctx, state.Spec.CheckoutPath, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return SourceUpdate{}, err
	}
	if strings.TrimSpace(string(commitBytes)) != state.AppliedCommit {
		return SourceUpdate{}, fmt.Errorf("%w: managed checkout revision differs from applied state", ErrConflict)
	}
	bundlePath := filepath.Join(state.Spec.CheckoutPath, filepath.FromSlash(state.Spec.BundlePath))
	bundle, provenance, err := LoadBundle(bundlePath)
	if err != nil {
		return SourceUpdate{}, err
	}
	if provenance.SHA256 != state.BundleSHA256 {
		return SourceUpdate{}, fmt.Errorf("%w: managed checkout bundle differs from applied state", ErrConflict)
	}
	materialized := MaterializeBundle(bundle)
	_, beforeLoadErr := Load(cleanConfig)
	before, _ := fileSHA256(cleanConfig)
	if err := Save(cleanConfig, materialized, true); err != nil {
		return SourceUpdate{}, err
	}
	after, err := fileSHA256(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	if after != state.LiveSHA256 {
		return SourceUpdate{}, fmt.Errorf("%w: restored config digest does not match applied state", ErrConflict)
	}
	status, err := SourceStatusWithGit(ctx, git, cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	return SourceUpdate{Changed: beforeLoadErr != nil || before != after, Status: status}, nil
}

// SourceGit is argv-based so no config value is ever interpreted by a shell.
type SourceGit interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type NativeSourceGit struct{ Binary string }

func (g NativeSourceGit) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	binary := strings.TrimSpace(g.Binary)
	if binary == "" {
		binary = "git"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = gitSourceEnvironment()
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(output.String())
		kind := ErrSourceGit
		lower := strings.ToLower(text)
		for _, marker := range []string{"permission denied", "authentication failed", "could not read username", "publickey", "access denied"} {
			if strings.Contains(lower, marker) {
				kind = ErrSourceAuth
				break
			}
		}
		return nil, fmt.Errorf("%w: git %s: %v: %s", kind, strings.Join(redactSourceGitArgs(args), " "), err, redactSourceGitText(text))
	}
	return output.Bytes(), nil
}

func DefaultSourceCheckout() (string, error) {
	if root := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); root != "" {
		return filepath.Join(root, "agentctl", "config-source"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "agentctl", "config-source"), nil
}

func SourceStatePath(configPath string) (string, error) {
	clean, err := cleanSavePath(configPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(clean), "config-source.json"), nil
}

func NormalizeSourceSpec(spec SourceSpec) (SourceSpec, error) {
	spec.Remote = strings.TrimSpace(spec.Remote)
	spec.Ref = strings.TrimSpace(spec.Ref)
	if spec.Ref == "" {
		spec.Ref = "main"
	}
	spec.BundlePath = strings.TrimSpace(spec.BundlePath)
	if spec.BundlePath == "" {
		spec.BundlePath = defaultSourceBundle
	}
	if spec.CheckoutPath == "" {
		checkout, err := DefaultSourceCheckout()
		if err != nil {
			return SourceSpec{}, err
		}
		spec.CheckoutPath = checkout
	}
	abs, err := filepath.Abs(filepath.Clean(spec.CheckoutPath))
	if err != nil {
		return SourceSpec{}, fmt.Errorf("resolve checkout path: %w", err)
	}
	spec.CheckoutPath = abs
	if abs == filepath.VolumeName(abs)+string(filepath.Separator) {
		return SourceSpec{}, errors.New("checkout path must not be a filesystem root")
	}
	if err := validateSourceRemote(spec.Remote); err != nil {
		return SourceSpec{}, err
	}
	if filepath.IsAbs(spec.Remote) {
		spec.Remote = filepath.Clean(spec.Remote)
	}
	if err := validateSourceRef(spec.Ref); err != nil {
		return SourceSpec{}, err
	}
	cleanBundle := filepath.ToSlash(filepath.Clean(spec.BundlePath))
	if filepath.IsAbs(spec.BundlePath) || strings.ContainsAny(spec.BundlePath, "\r\n\x00\\") || cleanBundle == "." || cleanBundle == ".." || strings.HasPrefix(cleanBundle, "../") || strings.HasPrefix(cleanBundle, ".git/") || cleanBundle == ".git" {
		return SourceSpec{}, errors.New("bundle path must be a repository-relative file outside .git")
	}
	spec.BundlePath = cleanBundle
	return spec, nil
}

func PlanSource(configPath string, spec SourceSpec, operation string) (SourcePlan, error) {
	cleanConfig, err := cleanSavePath(configPath)
	if err != nil {
		return SourcePlan{}, err
	}
	normalized, err := NormalizeSourceSpec(spec)
	if err != nil {
		return SourcePlan{}, err
	}
	statePath, err := SourceStatePath(cleanConfig)
	if err != nil {
		return SourcePlan{}, err
	}
	return SourcePlan{
		Operation: operation, Source: normalized, ConfigPath: cleanConfig, StatePath: statePath,
		PlanEffects:     SourceSideEffects{NetworkAccess: false, Mutates: false},
		ApplyEffects:    SourceSideEffects{NetworkAccess: true, Mutates: true},
		RemoteValidated: false,
		Safety:          []string{"plan validates local syntax and paths only; it does not verify remote access or bundle contents", "update is fast-forward only", "dirty managed checkouts fail closed", "live config drift fails closed", "Git and SSH own credentials"},
	}, nil
}

func InitSource(ctx context.Context, configPath string, spec SourceSpec, now time.Time) (SourceUpdate, error) {
	return InitSourceWithGit(ctx, NativeSourceGit{}, configPath, spec, now)
}

func InitSourceWithGit(ctx context.Context, git SourceGit, configPath string, spec SourceSpec, now time.Time) (SourceUpdate, error) {
	if git == nil {
		return SourceUpdate{}, errors.New("nil Git runner")
	}
	cleanConfig, err := cleanSavePath(configPath)
	if err != nil {
		return SourceUpdate{}, err
	}
	spec, err = NormalizeSourceSpec(spec)
	if err != nil {
		return SourceUpdate{}, err
	}
	lock, err := acquireSourceLock(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	defer lock.Close()
	statePath, err := SourceStatePath(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	if _, err := LoadSourceState(statePath); err == nil {
		return SourceUpdate{}, fmt.Errorf("%w: config source is already initialized", ErrConflict)
	} else if !errors.Is(err, ErrNotFound) {
		return SourceUpdate{}, err
	}
	if _, err := os.Lstat(spec.CheckoutPath); err == nil {
		return SourceUpdate{}, fmt.Errorf("%w: checkout path already exists", ErrConflict)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return SourceUpdate{}, fmt.Errorf("inspect checkout path: %w", err)
	}
	checkoutParent := filepath.Dir(spec.CheckoutPath)
	if err := ensureOwnerPath(checkoutParent); err != nil {
		return SourceUpdate{}, err
	}
	stagingPath := filepath.Join(checkoutParent, "."+filepath.Base(spec.CheckoutPath)+".tmp-"+randomSuffix())
	if err := os.Mkdir(stagingPath, 0o700); err != nil {
		return SourceUpdate{}, fmt.Errorf("create staged config source checkout: %w", err)
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(stagingPath)
		}
	}()
	stagedSpec := spec
	stagedSpec.CheckoutPath = stagingPath
	if _, err := git.Run(ctx, stagingPath, "init", "--quiet"); err != nil {
		return SourceUpdate{}, err
	}
	if _, err := git.Run(ctx, stagingPath, "remote", "add", "origin", spec.Remote); err != nil {
		return SourceUpdate{}, err
	}
	commit, bundle, bundleDigest, err := fetchSource(ctx, git, stagedSpec)
	if err != nil {
		return SourceUpdate{}, err
	}
	materialized := MaterializeBundle(bundle)
	configExisted := false
	if existing, loadErr := Load(cleanConfig); loadErr == nil {
		configExisted = true
		if !configsEqual(existing, materialized) {
			return SourceUpdate{}, fmt.Errorf("%w: existing live config differs from Git bundle", ErrConflict)
		}
	} else if !errors.Is(loadErr, ErrNotFound) {
		return SourceUpdate{}, loadErr
	}
	if _, err := git.Run(ctx, stagingPath, "checkout", "--detach", "--force", commit); err != nil {
		return SourceUpdate{}, err
	}
	if err := os.Rename(stagingPath, spec.CheckoutPath); err != nil {
		return SourceUpdate{}, fmt.Errorf("activate config source checkout: %w", err)
	}
	removeStaging = false
	rollbackCheckout := true
	defer func() {
		if rollbackCheckout {
			_ = os.RemoveAll(spec.CheckoutPath)
		}
	}()
	if err := Save(cleanConfig, materialized, false); err != nil {
		return SourceUpdate{}, err
	}
	liveDigest, err := fileSHA256(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	state := SourceState{SchemaVersion: SourceSchemaVersion, Spec: spec, AppliedCommit: commit, BundleSHA256: bundleDigest, LiveSHA256: liveDigest, AppliedAt: now.UTC()}
	if err := SaveSourceState(statePath, state, false); err != nil {
		_ = os.Remove(statePath)
		if !configExisted {
			_ = os.Remove(cleanConfig)
		}
		return SourceUpdate{}, err
	}
	rollbackCheckout = false
	status, err := SourceStatusWithGit(ctx, git, cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	return SourceUpdate{Changed: true, Status: status}, nil
}

func UpdateSource(ctx context.Context, configPath string, now time.Time) (SourceUpdate, error) {
	return UpdateSourceWithGit(ctx, NativeSourceGit{}, configPath, now)
}

func UpdateSourceWithGit(ctx context.Context, git SourceGit, configPath string, now time.Time) (SourceUpdate, error) {
	if git == nil {
		return SourceUpdate{}, errors.New("nil Git runner")
	}
	cleanConfig, err := cleanSavePath(configPath)
	if err != nil {
		return SourceUpdate{}, err
	}
	lock, err := acquireSourceLock(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	defer lock.Close()
	statePath, err := SourceStatePath(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	state, err := LoadSourceState(statePath)
	if err != nil {
		return SourceUpdate{}, err
	}
	currentLive, err := fileSHA256(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	if currentLive != state.LiveSHA256 {
		return SourceUpdate{}, fmt.Errorf("%w: live config drifted since the last source update", ErrConflict)
	}
	previousConfig, err := Load(cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	previousState := state
	if err := verifyManagedCheckout(ctx, git, state.Spec); err != nil {
		return SourceUpdate{}, err
	}
	commit, bundle, bundleDigest, err := fetchSource(ctx, git, state.Spec)
	if err != nil {
		return SourceUpdate{}, err
	}
	if commit != state.AppliedCommit {
		if _, err := git.Run(ctx, state.Spec.CheckoutPath, "merge-base", "--is-ancestor", state.AppliedCommit, commit); err != nil {
			return SourceUpdate{}, fmt.Errorf("%w: fetched revision is not a fast-forward of the applied commit", ErrConflict)
		}
	}
	materialized := MaterializeBundle(bundle)
	if commit == state.AppliedCommit && bundleDigest == state.BundleSHA256 {
		status, err := SourceStatusWithGit(ctx, git, cleanConfig)
		if err != nil {
			return SourceUpdate{}, err
		}
		return SourceUpdate{Changed: false, Status: status}, nil
	}
	if _, err := git.Run(ctx, state.Spec.CheckoutPath, "checkout", "--detach", "--force", commit); err != nil {
		return SourceUpdate{}, err
	}
	rollback := func(cause error) error {
		var rollbackErrors []error
		if restoreErr := SaveSourceState(statePath, previousState, true); restoreErr != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore config source state: %w", restoreErr))
		}
		if restoreErr := Save(cleanConfig, previousConfig, true); restoreErr != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore live config: %w", restoreErr))
		}
		if _, restoreErr := git.Run(ctx, state.Spec.CheckoutPath, "checkout", "--detach", "--force", previousState.AppliedCommit); restoreErr != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore managed checkout: %w", restoreErr))
		}
		if len(rollbackErrors) != 0 {
			return errors.Join(cause, fmt.Errorf("config source rollback incomplete: %w", errors.Join(rollbackErrors...)))
		}
		return cause
	}
	if err := Save(cleanConfig, materialized, true); err != nil {
		return SourceUpdate{}, rollback(err)
	}
	liveDigest, err := fileSHA256(cleanConfig)
	if err != nil {
		return SourceUpdate{}, rollback(err)
	}
	changed := commit != state.AppliedCommit || bundleDigest != state.BundleSHA256 || liveDigest != state.LiveSHA256
	state.AppliedCommit, state.BundleSHA256, state.LiveSHA256, state.AppliedAt = commit, bundleDigest, liveDigest, now.UTC()
	if err := SaveSourceState(statePath, state, true); err != nil {
		return SourceUpdate{}, rollback(err)
	}
	status, err := SourceStatusWithGit(ctx, git, cleanConfig)
	if err != nil {
		return SourceUpdate{}, err
	}
	return SourceUpdate{Changed: changed, Status: status}, nil
}

func SourceStatusReadOnly(ctx context.Context, configPath string) (SourceStatus, error) {
	return SourceStatusWithGit(ctx, NativeSourceGit{}, configPath)
}

func SourceStatusWithGit(ctx context.Context, git SourceGit, configPath string) (SourceStatus, error) {
	cleanConfig, err := cleanSavePath(configPath)
	if err != nil {
		return SourceStatus{}, err
	}
	statePath, err := SourceStatePath(cleanConfig)
	if err != nil {
		return SourceStatus{}, err
	}
	result := SourceStatus{ConfigPath: cleanConfig, StatePath: statePath, InSync: true}
	state, err := LoadSourceState(statePath)
	if errors.Is(err, ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return SourceStatus{}, err
	}
	result.Configured = true
	result.Source = state.Spec
	result.AppliedCommit = state.AppliedCommit
	result.BundleSHA256 = state.BundleSHA256
	result.ExpectedLiveSHA = state.LiveSHA256
	result.LastAppliedAt = &state.AppliedAt
	if _, loadErr := Load(cleanConfig); loadErr != nil {
		result.Drift = append(result.Drift, "live_config_unmanaged_or_invalid")
	} else if live, digestErr := fileSHA256(cleanConfig); digestErr == nil {
		result.LiveSHA256 = live
		if live != state.LiveSHA256 {
			result.Drift = append(result.Drift, "live_config")
		}
	} else {
		result.Drift = append(result.Drift, "live_config_missing_or_unreadable")
	}
	if git == nil {
		return SourceStatus{}, errors.New("nil Git runner")
	}
	if _, statErr := os.Lstat(state.Spec.CheckoutPath); statErr != nil {
		result.Drift = append(result.Drift, "checkout_missing_or_unreadable")
		return result, nil
	}
	commitBytes, commitErr := git.Run(ctx, state.Spec.CheckoutPath, "rev-parse", "HEAD^{commit}")
	if commitErr != nil {
		result.Drift = append(result.Drift, "checkout_revision")
	} else {
		result.CheckoutCommit = strings.TrimSpace(string(commitBytes))
		if result.CheckoutCommit != state.AppliedCommit {
			result.Drift = append(result.Drift, "checkout_revision")
		}
	}
	cleanBytes, cleanErr := git.Run(ctx, state.Spec.CheckoutPath, "status", "--porcelain", "--untracked-files=all")
	result.CheckoutClean = cleanErr == nil && strings.TrimSpace(string(cleanBytes)) == ""
	if !result.CheckoutClean {
		result.Drift = append(result.Drift, "checkout_dirty")
	}
	bundlePath := filepath.Join(state.Spec.CheckoutPath, filepath.FromSlash(state.Spec.BundlePath))
	if _, provenance, bundleErr := LoadBundle(bundlePath); bundleErr != nil {
		result.Drift = append(result.Drift, "checkout_bundle")
	} else {
		result.CheckoutSHA256 = provenance.SHA256
		if provenance.SHA256 != state.BundleSHA256 {
			result.Drift = append(result.Drift, "checkout_bundle")
		}
	}
	result.InSync = len(result.Drift) == 0
	return result, nil
}

func LoadSourceState(path string) (SourceState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return SourceState{}, ErrNotFound
	}
	if err != nil {
		return SourceState{}, fmt.Errorf("inspect config source state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return SourceState{}, fmt.Errorf("%w: source state must be a regular owner-only 0600 file", ErrUnmanaged)
	}
	if err := ensureOwnerPath(filepath.Dir(path)); err != nil {
		return SourceState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SourceState{}, fmt.Errorf("read config source state: %w", err)
	}
	var state SourceState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return SourceState{}, fmt.Errorf("parse config source state: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return SourceState{}, errors.New("parse config source state: trailing JSON document")
	}
	if state.SchemaVersion != SourceSchemaVersion || state.AppliedCommit == "" || state.BundleSHA256 == "" || state.LiveSHA256 == "" || state.AppliedAt.IsZero() {
		return SourceState{}, errors.New("invalid config source state")
	}
	normalized, err := NormalizeSourceSpec(state.Spec)
	if err != nil || normalized != state.Spec {
		return SourceState{}, errors.New("invalid non-canonical config source state")
	}
	return state, nil
}

func SaveSourceState(path string, state SourceState, overwrite bool) error {
	if state.SchemaVersion != SourceSchemaVersion {
		return errors.New("invalid config source state schema")
	}
	parent := filepath.Dir(path)
	if err := ensureOwnerPath(parent); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%w: source state destination is unmanaged", ErrUnmanaged)
		}
		if !overwrite {
			return fmt.Errorf("%w: source state already exists", ErrConflict)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect config source state: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config source state: %w", err)
	}
	tmp := path + ".tmp-" + randomSuffix()
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, ConfigFileMode)
	if err != nil {
		return fmt.Errorf("create config source temporary file: %w", err)
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("write config source state: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync config source state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config source state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace config source state: %w", err)
	}
	remove = false
	return syncDirectory(parent)
}

func fetchSource(ctx context.Context, git SourceGit, spec SourceSpec) (string, Bundle, string, error) {
	if _, err := git.Run(ctx, spec.CheckoutPath, "fetch", "--no-tags", "--prune", "origin", spec.Ref); err != nil {
		return "", Bundle{}, "", err
	}
	commitBytes, err := git.Run(ctx, spec.CheckoutPath, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", Bundle{}, "", err
	}
	commit := strings.TrimSpace(string(commitBytes))
	bundleBytes, err := git.Run(ctx, spec.CheckoutPath, "show", commit+":"+spec.BundlePath)
	if err != nil {
		return "", Bundle{}, "", fmt.Errorf("read config bundle from fetched revision: %w", err)
	}
	if int64(len(bundleBytes)) > BundleMaxBytes {
		return "", Bundle{}, "", fmt.Errorf("config bundle exceeds %d-byte limit", BundleMaxBytes)
	}
	bundle, err := decodeBundle(bundleBytes)
	if err != nil {
		return "", Bundle{}, "", err
	}
	digest := sha256.Sum256(bundleBytes)
	return commit, bundle, hex.EncodeToString(digest[:]), nil
}

func verifyManagedCheckout(ctx context.Context, git SourceGit, spec SourceSpec) error {
	remoteBytes, err := git.Run(ctx, spec.CheckoutPath, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(remoteBytes)) != spec.Remote {
		return fmt.Errorf("%w: managed checkout remote differs from source state", ErrConflict)
	}
	statusBytes, err := git.Run(ctx, spec.CheckoutPath, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(statusBytes)) != "" {
		return fmt.Errorf("%w: managed checkout has local changes", ErrConflict)
	}
	return nil
}

func MaterializeBundle(bundle Bundle) Config {
	cfg := Config{SchemaVersion: SchemaVersion, DefaultProfile: bundle.DefaultProfile, Profiles: make(map[string]Profile, len(bundle.Profiles))}
	for name, profile := range bundle.Profiles {
		cfg.Profiles[name] = profile.profile()
	}
	return cfg
}

func configsEqual(a, b Config) bool {
	aBytes, aErr := marshalConfig(a)
	bBytes, bErr := marshalConfig(b)
	return aErr == nil && bErr == nil && bytes.Equal(aBytes, bBytes)
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read digest input: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validateSourceRemote(remote string) error {
	if remote == "" || strings.ContainsAny(remote, "\r\n\x00") {
		return errors.New("source remote is required and must not contain control characters")
	}
	if filepath.IsAbs(remote) {
		return nil
	}
	if scpRemotePattern.MatchString(remote) {
		return nil
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("source remote must be an absolute HTTPS/SSH URL, SCP-style SSH remote, or absolute local path without credentials")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "ssh" {
		return errors.New("source remote scheme must be https or ssh")
	}
	return nil
}

func validateSourceRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || strings.ContainsAny(ref, " ~^:?*[\\\r\n\x00") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		return errors.New("source ref is not a safe Git branch or tag name")
	}
	return nil
}

func gitSourceEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GIT_TERMINAL_PROMPT=") || strings.HasPrefix(entry, "GIT_OPTIONAL_LOCKS=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
}

func redactSourceGitArgs(args []string) []string {
	result := append([]string(nil), args...)
	for i, value := range result {
		if strings.Contains(value, "://") || scpRemotePattern.MatchString(value) {
			result[i] = "<redacted-remote>"
		}
	}
	return result
}

func redactSourceGitText(value string) string {
	fields := strings.Fields(value)
	for i, field := range fields {
		if strings.Contains(field, "://") || scpRemotePattern.MatchString(field) {
			fields[i] = "<redacted-remote>"
		}
	}
	return strings.Join(fields, " ")
}

type sourceLock struct{ file *os.File }

func acquireSourceLock(configPath string) (*sourceLock, error) {
	parent := filepath.Dir(configPath)
	if err := ensureOwnerPath(parent); err != nil {
		return nil, err
	}
	path := filepath.Join(parent, "config-source.lock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%w: config source lock is unmanaged", ErrUnmanaged)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect config source lock: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, ConfigFileMode)
	if err != nil {
		return nil, fmt.Errorf("open config source lock: %w", err)
	}
	if err := file.Chmod(ConfigFileMode); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure config source lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: another config source operation is active", ErrConflict)
		}
		return nil, fmt.Errorf("lock config source: %w", err)
	}
	return &sourceLock{file: file}, nil
}

func (l *sourceLock) Close() {
	if l == nil || l.file == nil {
		return
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
}
