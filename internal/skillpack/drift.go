package skillpack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
)

type DriftFile struct {
	Path        string `json:"path"`
	State       string `json:"state"`
	HubSHA256   string `json:"hub_sha256,omitempty"`
	LocalSHA256 string `json:"local_sha256,omitempty"`
	HubBytes    int    `json:"hub_bytes,omitempty"`
	LocalBytes  int    `json:"local_bytes,omitempty"`
}

type DriftReview struct {
	Skill       string      `json:"skill"`
	Harnesses   []string    `json:"harnesses"`
	Destination string      `json:"destination"`
	State       string      `json:"state"`
	Reason      string      `json:"reason"`
	Files       []DriftFile `json:"files"`
}

type DriftRestore struct {
	Review     DriftReview `json:"review"`
	BackupPath string      `json:"backup_path,omitempty"`
	Applied    bool        `json:"applied"`
}

type DriftProposal struct {
	Review       DriftReview `json:"review"`
	WorktreePath string      `json:"worktree_path,omitempty"`
	Branch       string      `json:"branch,omitempty"`
	Applied      bool        `json:"applied"`
}

func ReviewDrift(options Options, name, harness string) (DriftReview, error) {
	action, skill, err := resolveDrift(options, name, harness)
	if err != nil {
		return DriftReview{}, err
	}
	local, _, err := readInstalledTree(action.Destination)
	if err != nil {
		return DriftReview{}, err
	}
	return DriftReview{Skill: name, Harnesses: action.Harnesses, Destination: action.Destination, State: action.State, Reason: action.Reason, Files: compareAssets(skill.Assets, local)}, nil
}

func RestoreDrift(options Options, name, harness string, apply bool, now time.Time) (DriftRestore, error) {
	action, skill, err := resolveDrift(options, name, harness)
	if err != nil {
		return DriftRestore{}, err
	}
	local, _, err := readInstalledTree(action.Destination)
	if err != nil {
		return DriftRestore{}, err
	}
	review := DriftReview{Skill: name, Harnesses: action.Harnesses, Destination: action.Destination, State: action.State, Reason: action.Reason, Files: compareAssets(skill.Assets, local)}
	result := DriftRestore{Review: review}
	if !apply {
		return result, nil
	}
	lock, err := acquireLock(options.Home)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	// Re-resolve under the mutation lock so a concurrent local edit cannot be
	// overwritten based on stale review evidence.
	action, skill, err = resolveDrift(options, name, harness)
	if err != nil {
		return result, err
	}
	backupRoot := filepath.Join(options.Home, ".local", "share", "agentctl", "skill-backups", now.UTC().Format("20060102T150405Z"), strings.Join(action.Harnesses, "+"), name)
	if _, err := os.Lstat(backupRoot); err == nil {
		return result, fmt.Errorf("%w: backup destination already exists", ErrConflict)
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err := copyInstalledTree(action.Destination, backupRoot); err != nil {
		return result, err
	}
	_, _, manifestDigest, _, err := load(options.Source)
	if err != nil {
		return result, err
	}
	if err := writeSkill(action.Destination, skill, action.Harnesses, options.Source, manifestDigest); err != nil {
		return result, err
	}
	result.BackupPath, result.Applied = backupRoot, true
	return result, nil
}

func ProposeDrift(ctx context.Context, git config.SourceGit, options Options, name, harness string, apply bool, now time.Time) (DriftProposal, error) {
	action, _, err := resolveDrift(options, name, harness)
	if err != nil {
		return DriftProposal{}, err
	}
	local, _, err := readInstalledTree(action.Destination)
	if err != nil {
		return DriftProposal{}, err
	}
	_, manifest, _, prepared, err := load(options.Source)
	if err != nil {
		return DriftProposal{}, err
	}
	var skill preparedSkill
	for _, candidate := range prepared {
		if candidate.Name == name {
			skill = candidate
			break
		}
	}
	review := DriftReview{Skill: name, Harnesses: action.Harnesses, Destination: action.Destination, State: action.State, Reason: action.Reason, Files: compareAssets(skill.Assets, local)}
	result := DriftProposal{Review: review}
	if !apply {
		return result, nil
	}
	if git == nil {
		git = config.NativeSourceGit{}
	}
	stamp := now.UTC().Format("20060102-150405")
	branch := "agentctl/skill-" + name + "-" + stamp
	proposalBase := strings.TrimSpace(options.ProposalRoot)
	if proposalBase == "" {
		proposalBase = filepath.Join(options.Home, ".local", "share", "agentctl", "skill-proposals")
	}
	if !filepath.IsAbs(proposalBase) {
		return result, fmt.Errorf("%w: proposal root must be absolute", ErrInvalidInput)
	}
	proposalRoot := filepath.Join(filepath.Clean(proposalBase), name+"-"+stamp)
	if err := os.MkdirAll(filepath.Dir(proposalRoot), 0o700); err != nil {
		return result, err
	}
	if _, err := git.Run(ctx, options.Source.RepositoryRoot, "worktree", "add", "-b", branch, proposalRoot, options.Source.Commit); err != nil {
		return result, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_, _ = git.Run(context.Background(), options.Source.RepositoryRoot, "worktree", "remove", "--force", proposalRoot)
			_, _ = git.Run(context.Background(), options.Source.RepositoryRoot, "branch", "-D", branch)
		}
	}()
	path := ""
	for _, declared := range manifest.Skills {
		if declared.Name == name {
			path = declared.Path
			break
		}
	}
	if path == "" {
		return result, errors.New("skill is absent from the pinned manifest")
	}
	target := filepath.Join(proposalRoot, filepath.FromSlash(path))
	if err := os.RemoveAll(target); err != nil {
		return result, err
	}
	if err := copyInstalledTree(action.Destination, target); err != nil {
		return result, err
	}
	if _, _, _, _, err := load(Source{RepositoryRoot: proposalRoot, Remote: options.Source.Remote, Commit: options.Source.Commit, ManifestPath: options.Source.ManifestPath}); err != nil {
		return result, fmt.Errorf("validate proposal: %w", err)
	}
	cleanup = false
	result.WorktreePath, result.Branch, result.Applied = proposalRoot, branch, true
	return result, nil
}

func resolveDrift(options Options, name, harness string) (Action, preparedSkill, error) {
	if !skillNamePattern.MatchString(name) {
		return Action{}, preparedSkill{}, fmt.Errorf("%w: invalid skill name", ErrInvalidInput)
	}
	report, err := Plan(options)
	if err != nil {
		return Action{}, preparedSkill{}, err
	}
	var matches []Action
	for _, action := range report.Actions {
		if action.Skill != name || action.State != "drifted" {
			continue
		}
		if harness != "" && !containsString(action.Harnesses, harness) {
			continue
		}
		matches = append(matches, action)
	}
	if len(matches) == 0 {
		return Action{}, preparedSkill{}, fmt.Errorf("%w: skill %q has no matching managed drift", ErrConflict, name)
	}
	if len(matches) != 1 {
		return Action{}, preparedSkill{}, fmt.Errorf("%w: skill %q is drifted in multiple roots; select --harness", ErrConflict, name)
	}
	_, _, _, prepared, err := load(options.Source)
	if err != nil {
		return Action{}, preparedSkill{}, err
	}
	for _, skill := range prepared {
		if skill.Name == name {
			return matches[0], skill, nil
		}
	}
	return Action{}, preparedSkill{}, errors.New("skill missing from prepared pack")
}

func compareAssets(hub, local []fileAsset) []DriftFile {
	hubMap, localMap := map[string]fileAsset{}, map[string]fileAsset{}
	for _, asset := range hub {
		hubMap[asset.Path] = asset
	}
	for _, asset := range local {
		localMap[asset.Path] = asset
	}
	paths := map[string]bool{}
	for path := range hubMap {
		paths[path] = true
	}
	for path := range localMap {
		paths[path] = true
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	result := make([]DriftFile, 0, len(ordered))
	for _, path := range ordered {
		h, hasHub := hubMap[path]
		l, hasLocal := localMap[path]
		entry := DriftFile{Path: path}
		switch {
		case !hasHub:
			entry.State = "local_only"
		case !hasLocal:
			entry.State = "hub_only"
		case string(h.Data) == string(l.Data) && normalizedMode(h.Mode) == normalizedMode(l.Mode):
			continue
		default:
			entry.State = "modified"
		}
		if hasHub {
			entry.HubSHA256, entry.HubBytes = assetSHA(h.Data), len(h.Data)
		}
		if hasLocal {
			entry.LocalSHA256, entry.LocalBytes = assetSHA(l.Data), len(l.Data)
		}
		result = append(result, entry)
	}
	return result
}

func assetSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func copyInstalledTree(source, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!entry.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("drifted skill contains an unsupported file")
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) == MarkerName {
			return nil
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o600)
		if info.Mode()&0o100 != 0 {
			mode = 0o700
		}
		return os.WriteFile(target, data, mode)
	})
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
