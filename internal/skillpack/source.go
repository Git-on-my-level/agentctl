package skillpack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
)

const hubStateSchemaVersion = 1

type HubSelection struct {
	Remote       string `json:"remote"`
	Ref          string `json:"ref"`
	ManifestPath string `json:"manifest_path"`
}

type HubState struct {
	SchemaVersion int          `json:"schema_version"`
	Selection     HubSelection `json:"selection"`
	CheckoutPath  string       `json:"checkout_path"`
	AppliedCommit string       `json:"applied_commit"`
	AppliedAt     time.Time    `json:"applied_at"`
	CheckedOn     string       `json:"checked_on,omitempty"`
}

type HubStatus struct {
	Configured     bool         `json:"configured"`
	InSync         bool         `json:"in_sync"`
	Selection      HubSelection `json:"selection"`
	CheckoutPath   string       `json:"checkout_path"`
	StatePath      string       `json:"state_path"`
	AppliedCommit  string       `json:"applied_commit,omitempty"`
	CheckoutCommit string       `json:"checkout_commit,omitempty"`
	CheckoutClean  bool         `json:"checkout_clean"`
	CheckedOn      string       `json:"checked_on,omitempty"`
	Drift          []string     `json:"drift,omitempty"`
}

type HubUpdate struct {
	Changed bool      `json:"changed"`
	Status  HubStatus `json:"status"`
}

func NormalizeHubSelection(input HubSelection) (HubSelection, error) {
	input.Remote = strings.TrimSpace(input.Remote)
	input.Ref = strings.TrimSpace(input.Ref)
	if input.Ref == "" {
		input.Ref = "main"
	}
	input.ManifestPath = strings.TrimSpace(input.ManifestPath)
	contract := config.Skills{Source: config.SkillSource{Remote: input.Remote, Ref: input.Ref, ManifestPath: input.ManifestPath}, UpdatePolicy: "manual"}
	if err := contract.Validate(); err != nil {
		return HubSelection{}, err
	}
	clean, err := cleanRelativePath(input.ManifestPath, "manifest path")
	if err != nil {
		return HubSelection{}, err
	}
	input.ManifestPath = clean
	return input, nil
}

func DefaultHubPaths(home string) (checkoutPath, statePath string, err error) {
	home, err = cleanExistingDirectory(home, "home")
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, ".local", "share", "agentctl", "skill-source"), filepath.Join(home, ".local", "state", "agentctl", "skill-source.json"), nil
}

func HubSourceStatusReadOnly(ctx context.Context, home string, selection HubSelection) (HubStatus, error) {
	return HubSourceStatusWithGit(ctx, config.NativeSourceGit{}, home, selection)
}

func HubSourceStatusWithGit(ctx context.Context, git config.SourceGit, home string, selection HubSelection) (HubStatus, error) {
	selection, err := NormalizeHubSelection(selection)
	if err != nil {
		return HubStatus{}, err
	}
	checkoutPath, statePath, err := DefaultHubPaths(home)
	if err != nil {
		return HubStatus{}, err
	}
	status := HubStatus{InSync: true, Selection: selection, CheckoutPath: checkoutPath, StatePath: statePath}
	state, err := loadHubState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return status, nil
	}
	if err != nil {
		return HubStatus{}, err
	}
	status.Configured = true
	status.AppliedCommit = state.AppliedCommit
	status.CheckedOn = state.CheckedOn
	if state.Selection != selection || state.CheckoutPath != checkoutPath {
		status.Drift = append(status.Drift, "selection")
	}
	if git == nil {
		return HubStatus{}, errors.New("nil Git runner")
	}
	if _, err := os.Lstat(checkoutPath); err != nil {
		status.Drift = append(status.Drift, "checkout_missing_or_unreadable")
		status.InSync = false
		return status, nil
	}
	if output, err := git.Run(ctx, checkoutPath, "status", "--porcelain=v1", "--untracked-files=all"); err != nil || len(bytes.TrimSpace(output)) != 0 {
		status.Drift = append(status.Drift, "checkout_dirty")
	} else {
		status.CheckoutClean = true
	}
	if output, err := git.Run(ctx, checkoutPath, "rev-parse", "HEAD^{commit}"); err != nil {
		status.Drift = append(status.Drift, "checkout_revision")
	} else {
		status.CheckoutCommit = strings.TrimSpace(string(output))
		if status.CheckoutCommit != state.AppliedCommit {
			status.Drift = append(status.Drift, "checkout_revision")
		}
	}
	if output, err := git.Run(ctx, checkoutPath, "remote", "get-url", "origin"); err != nil || strings.TrimSpace(string(output)) != selection.Remote {
		status.Drift = append(status.Drift, "checkout_remote")
	}
	if status.CheckoutClean && status.CheckoutCommit == state.AppliedCommit {
		if _, _, _, _, err := load(Source{RepositoryRoot: checkoutPath, Remote: selection.Remote, Commit: state.AppliedCommit, ManifestPath: selection.ManifestPath}); err != nil {
			status.Drift = append(status.Drift, "manifest_or_skill_tree")
		}
	}
	status.InSync = len(status.Drift) == 0
	return status, nil
}

func UpdateHubSource(ctx context.Context, home string, selection HubSelection, now time.Time) (HubUpdate, error) {
	return UpdateHubSourceWithGit(ctx, config.NativeSourceGit{}, home, selection, now)
}

func UpdateHubSourceWithGit(ctx context.Context, git config.SourceGit, home string, selection HubSelection, now time.Time) (HubUpdate, error) {
	if git == nil {
		return HubUpdate{}, errors.New("nil Git runner")
	}
	selection, err := NormalizeHubSelection(selection)
	if err != nil {
		return HubUpdate{}, err
	}
	checkoutPath, statePath, err := DefaultHubPaths(home)
	if err != nil {
		return HubUpdate{}, err
	}
	lock, err := acquireLock(home)
	if err != nil {
		return HubUpdate{}, err
	}
	defer lock.Close()
	state, err := loadHubState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		if err := initializeHubSource(ctx, git, checkoutPath, selection); err != nil {
			return HubUpdate{}, err
		}
		commit, err := git.Run(ctx, checkoutPath, "rev-parse", "HEAD^{commit}")
		if err != nil {
			return HubUpdate{}, err
		}
		state = HubState{SchemaVersion: hubStateSchemaVersion, Selection: selection, CheckoutPath: checkoutPath, AppliedCommit: strings.TrimSpace(string(commit)), AppliedAt: now.UTC(), CheckedOn: now.UTC().Format("2006-01-02")}
		if err := saveHubState(statePath, state, false); err != nil {
			removeErr := os.RemoveAll(checkoutPath)
			if removeErr != nil {
				return HubUpdate{}, errors.Join(err, fmt.Errorf("remove uncommitted Skill Hub checkout: %w", removeErr))
			}
			return HubUpdate{}, err
		}
		status, err := HubSourceStatusWithGit(ctx, git, home, selection)
		return HubUpdate{Changed: true, Status: status}, err
	}
	if err != nil {
		return HubUpdate{}, err
	}
	if state.Selection.Remote != selection.Remote || state.Selection.Ref != selection.Ref {
		return HubUpdate{}, fmt.Errorf("%w: changing the Skill Hub remote or ref requires explicit source reinitialization", ErrConflict)
	}
	status, err := HubSourceStatusWithGit(ctx, git, home, state.Selection)
	if err != nil {
		return HubUpdate{}, err
	}
	if !status.InSync {
		return HubUpdate{}, fmt.Errorf("%w: Skill Hub checkout drifted: %s", ErrConflict, strings.Join(status.Drift, ","))
	}
	if _, err := git.Run(ctx, checkoutPath, "fetch", "--no-tags", "--prune", "origin", selection.Ref); err != nil {
		return HubUpdate{}, err
	}
	output, err := git.Run(ctx, checkoutPath, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return HubUpdate{}, err
	}
	commit := strings.TrimSpace(string(output))
	if commit == state.AppliedCommit {
		if _, _, _, _, err := load(Source{RepositoryRoot: checkoutPath, Remote: selection.Remote, Commit: commit, ManifestPath: selection.ManifestPath}); err != nil {
			return HubUpdate{}, fmt.Errorf("validate selected Skill Hub manifest: %w", err)
		}
		changed := state.Selection.ManifestPath != selection.ManifestPath
		state.Selection = selection
		state.CheckedOn = now.UTC().Format("2006-01-02")
		if err := saveHubState(statePath, state, true); err != nil {
			return HubUpdate{}, err
		}
		status, err = HubSourceStatusWithGit(ctx, git, home, selection)
		return HubUpdate{Changed: changed, Status: status}, err
	}
	if _, err := git.Run(ctx, checkoutPath, "merge-base", "--is-ancestor", state.AppliedCommit, commit); err != nil {
		return HubUpdate{}, fmt.Errorf("%w: fetched Skill Hub revision is not a fast-forward", ErrConflict)
	}
	if _, err := git.Run(ctx, checkoutPath, "checkout", "--detach", "--force", commit); err != nil {
		return HubUpdate{}, err
	}
	rollback := func(cause error) error {
		_, restoreErr := git.Run(ctx, checkoutPath, "checkout", "--detach", "--force", state.AppliedCommit)
		return errors.Join(cause, restoreErr)
	}
	if _, _, _, _, err := load(Source{RepositoryRoot: checkoutPath, Remote: selection.Remote, Commit: commit, ManifestPath: selection.ManifestPath}); err != nil {
		return HubUpdate{}, rollback(fmt.Errorf("validate fetched Skill Hub: %w", err))
	}
	state.Selection = selection
	state.AppliedCommit, state.AppliedAt, state.CheckedOn = commit, now.UTC(), now.UTC().Format("2006-01-02")
	if err := saveHubState(statePath, state, true); err != nil {
		return HubUpdate{}, rollback(err)
	}
	status, err = HubSourceStatusWithGit(ctx, git, home, selection)
	return HubUpdate{Changed: true, Status: status}, err
}

// HubUpdateDue is a read-only daily scheduling check. Missing state is due;
// malformed state fails closed and is not considered due automatically.
func HubUpdateDue(home string, now time.Time) bool {
	_, statePath, err := DefaultHubPaths(home)
	if err != nil {
		return false
	}
	state, err := loadHubState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	return state.CheckedOn != now.UTC().Format("2006-01-02")
}

func initializeHubSource(ctx context.Context, git config.SourceGit, checkoutPath string, selection HubSelection) error {
	if _, err := os.Lstat(checkoutPath); err == nil {
		return fmt.Errorf("%w: Skill Hub checkout path already exists", ErrConflict)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(checkoutPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if err := validateNoSymlinkComponents(parent); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".skill-source-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if _, err := git.Run(ctx, stage, "init", "--quiet"); err != nil {
		return err
	}
	if _, err := git.Run(ctx, stage, "remote", "add", "origin", selection.Remote); err != nil {
		return err
	}
	if _, err := git.Run(ctx, stage, "fetch", "--no-tags", "--prune", "origin", selection.Ref); err != nil {
		return err
	}
	output, err := git.Run(ctx, stage, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return err
	}
	commit := strings.TrimSpace(string(output))
	if _, err := git.Run(ctx, stage, "checkout", "--detach", "--force", commit); err != nil {
		return err
	}
	if _, _, _, _, err := load(Source{RepositoryRoot: stage, Remote: selection.Remote, Commit: commit, ManifestPath: selection.ManifestPath}); err != nil {
		return fmt.Errorf("validate fetched Skill Hub: %w", err)
	}
	return os.Rename(stage, checkoutPath)
}

func loadHubState(path string) (HubState, error) {
	data, err := readRegularBounded(path, maxManifestBytes)
	if err != nil {
		return HubState{}, err
	}
	var state HubState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return HubState{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return HubState{}, errors.New("Skill Hub state has trailing content")
	}
	selection, selectionErr := NormalizeHubSelection(state.Selection)
	if state.SchemaVersion != hubStateSchemaVersion || !commitPattern.MatchString(state.AppliedCommit) || state.CheckoutPath == "" || !filepath.IsAbs(state.CheckoutPath) || filepath.Clean(state.CheckoutPath) != state.CheckoutPath || selectionErr != nil || selection != state.Selection {
		return HubState{}, errors.New("invalid Skill Hub state")
	}
	return state, nil
}

func saveHubState(path string, state HubState, replace bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := validateNoSymlinkComponents(filepath.Dir(path)); err != nil {
		return err
	}
	if !replace {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%w: Skill Hub state already exists", ErrConflict)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".skill-source-state-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func SourceFromHubStatus(status HubStatus) Source {
	return Source{RepositoryRoot: status.CheckoutPath, Remote: status.Selection.Remote, Commit: status.AppliedCommit, ManifestPath: status.Selection.ManifestPath}
}
