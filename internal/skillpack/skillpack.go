// Package skillpack validates and reconciles reviewed skill trees from an
// already-pinned agentctl config-source checkout into native harness roots.
package skillpack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	SchemaVersion    = 1
	DefaultManifest  = "skill-pack.json"
	MarkerName       = ".agentctl-skill.json"
	maxManifestBytes = 1 << 20
	maxSkillBytes    = 16 << 20
	maxSkillFiles    = 512
)

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

var (
	ErrConflict          = errors.New("managed skill conflict")
	ErrInvalidInput      = errors.New("invalid skill-pack input")
	ErrUnsupportedSchema = errors.New("unsupported skill-pack schema")
)

type Manifest struct {
	SchemaVersion int     `json:"schema_version"`
	Skills        []Skill `json:"skills"`
}

type Skill struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Targets []string `json:"targets"`
}

type Source struct {
	RepositoryRoot string `json:"repository_root"`
	Remote         string `json:"remote"`
	Commit         string `json:"commit"`
	ManifestPath   string `json:"manifest_path"`
}

type Options struct {
	Source            Source
	Home              string
	DetectedHarnesses []string
}

type Action struct {
	Skill       string   `json:"skill"`
	Harnesses   []string `json:"harnesses"`
	Destination string   `json:"destination,omitempty"`
	State       string   `json:"state"`
	Reason      string   `json:"reason,omitempty"`
	Changed     bool     `json:"changed"`
	Digest      string   `json:"content_sha256,omitempty"`
}

type Report struct {
	SchemaVersion  int      `json:"schema_version"`
	Healthy        bool     `json:"healthy"`
	Source         Source   `json:"source"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	Actions        []Action `json:"actions"`
	Changed        int      `json:"changed"`
	Applied        int      `json:"applied"`
	Unsupported    int      `json:"unsupported"`
	Conflicts      int      `json:"conflicts"`
}

type marker struct {
	SchemaVersion  int      `json:"schema_version"`
	ManagedBy      string   `json:"managed_by"`
	Skill          string   `json:"skill"`
	Harnesses      []string `json:"harnesses"`
	SourceRemote   string   `json:"source_remote"`
	SourceCommit   string   `json:"source_commit"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	ContentSHA256  string   `json:"content_sha256"`
}

type fileAsset struct {
	Path string
	Mode fs.FileMode
	Data []byte
}

type preparedSkill struct {
	Skill
	Assets []fileAsset
	Digest string
}

// Plan performs no writes and no network access. RepositoryRoot must already
// be pinned and independently verified by the caller.
func Plan(options Options) (Report, error) {
	source, manifest, manifestDigest, prepared, err := load(options.Source)
	if err != nil {
		return Report{}, err
	}
	home, err := cleanExistingDirectory(options.Home, "home")
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	detected := stringSet(options.DetectedHarnesses)
	report := Report{SchemaVersion: SchemaVersion, Healthy: true, Source: source, ManifestSHA256: manifestDigest, Actions: []Action{}}
	_ = manifest
	for _, skill := range prepared {
		groups := map[string][]string{}
		for _, harness := range skill.Targets {
			if harness == "multica" {
				report.Actions = append(report.Actions, Action{Skill: skill.Name, Harnesses: []string{harness}, State: "unsupported", Reason: "Multica skills are workspace/server scoped and require an advertised runtime-bundle installer", Digest: skill.Digest})
				report.Healthy = false
				report.Unsupported++
				continue
			}
			if !detected[harness] {
				report.Actions = append(report.Actions, Action{Skill: skill.Name, Harnesses: []string{harness}, State: "not_detected", Reason: "native harness is not detected on this host", Digest: skill.Digest})
				continue
			}
			root := harnessRoot(home, harness)
			if root == "" {
				report.Actions = append(report.Actions, Action{Skill: skill.Name, Harnesses: []string{harness}, State: "unsupported", Reason: "native harness has no supported canonical skill root", Digest: skill.Digest})
				report.Healthy = false
				report.Unsupported++
				continue
			}
			groups[root] = append(groups[root], harness)
		}
		roots := sortedKeys(groups)
		for _, root := range roots {
			harnesses := uniqueSorted(groups[root])
			destination := filepath.Join(root, skill.Name)
			if err := validateNoSymlinkComponents(root); err != nil {
				report.Actions = append(report.Actions, Action{Skill: skill.Name, Harnesses: harnesses, Destination: destination, State: "conflict", Reason: err.Error(), Digest: skill.Digest})
				report.Healthy = false
				report.Conflicts++
				continue
			}
			action := inspect(destination, skill, harnesses, source, manifestDigest)
			report.Actions = append(report.Actions, action)
			if action.Changed {
				report.Changed++
			}
			if action.State == "conflict" || action.State == "drifted" {
				report.Healthy = false
				report.Conflicts++
			}
		}
	}
	sort.Slice(report.Actions, func(i, j int) bool {
		if report.Actions[i].Skill != report.Actions[j].Skill {
			return report.Actions[i].Skill < report.Actions[j].Skill
		}
		if report.Actions[i].Destination != report.Actions[j].Destination {
			return report.Actions[i].Destination < report.Actions[j].Destination
		}
		return strings.Join(report.Actions[i].Harnesses, ",") < strings.Join(report.Actions[j].Harnesses, ",")
	})
	return report, nil
}

// Reconcile applies each declared skill atomically. The pack is preflighted in
// full before the first write; a later filesystem failure can leave earlier
// skills applied, and an idempotent retry converges the remaining actions.
func Reconcile(options Options) (Report, error) {
	lock, err := acquireLock(options.Home)
	if err != nil {
		return Report{}, err
	}
	defer lock.Close()
	report, err := Plan(options)
	if err != nil {
		return Report{}, err
	}
	if !report.Healthy {
		return report, fmt.Errorf("%w: reconcile refused unsupported or conflicting actions", ErrConflict)
	}
	_, _, _, prepared, err := load(options.Source)
	if err != nil {
		return Report{}, err
	}
	byName := make(map[string]preparedSkill, len(prepared))
	for _, skill := range prepared {
		byName[skill.Name] = skill
	}
	for _, action := range report.Actions {
		if !action.Changed || action.Destination == "" {
			continue
		}
		skill := byName[action.Skill]
		if err := writeSkill(action.Destination, skill, action.Harnesses, report.Source, report.ManifestSHA256); err != nil {
			return report, fmt.Errorf("reconcile skill %q: %w", action.Skill, err)
		}
		report.Applied++
	}
	final, err := Plan(options)
	if err != nil {
		return report, err
	}
	final.Applied = report.Applied
	return final, nil
}

func load(input Source) (Source, Manifest, string, []preparedSkill, error) {
	source, err := normalizeSource(input)
	if err != nil {
		return Source{}, Manifest{}, "", nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	manifestFile := filepath.Join(source.RepositoryRoot, filepath.FromSlash(source.ManifestPath))
	if err := validateContainedPath(source.RepositoryRoot, manifestFile); err != nil {
		return Source{}, Manifest{}, "", nil, fmt.Errorf("%w: manifest path: %v", ErrInvalidInput, err)
	}
	data, err := readRegularBounded(manifestFile, maxManifestBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
			return Source{}, Manifest{}, "", nil, fmt.Errorf("%w: read skill-pack manifest: %v", ErrInvalidInput, err)
		}
		return Source{}, Manifest{}, "", nil, fmt.Errorf("read skill-pack manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Source{}, Manifest{}, "", nil, fmt.Errorf("%w: parse skill-pack manifest: %v", ErrInvalidInput, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Source{}, Manifest{}, "", nil, fmt.Errorf("%w: parse skill-pack manifest: trailing JSON document", ErrInvalidInput)
	}
	if err := validateManifest(manifest); err != nil {
		if errors.Is(err, ErrUnsupportedSchema) {
			return Source{}, Manifest{}, "", nil, err
		}
		return Source{}, Manifest{}, "", nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	sum := sha256.Sum256(data)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])
	prepared := make([]preparedSkill, 0, len(manifest.Skills))
	for _, skill := range manifest.Skills {
		assets, digest, err := readSkillTree(source.RepositoryRoot, skill.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return Source{}, Manifest{}, "", nil, fmt.Errorf("skill %q: %w", skill.Name, err)
			}
			return Source{}, Manifest{}, "", nil, fmt.Errorf("%w: skill %q: %v", ErrInvalidInput, skill.Name, err)
		}
		prepared = append(prepared, preparedSkill{Skill: skill, Assets: assets, Digest: digest})
	}
	return source, manifest, manifestDigest, prepared, nil
}

func normalizeSource(source Source) (Source, error) {
	root, err := cleanExistingDirectory(source.RepositoryRoot, "repository root")
	if err != nil {
		return Source{}, err
	}
	source.RepositoryRoot = root
	source.Remote = strings.TrimSpace(source.Remote)
	source.Commit = strings.TrimSpace(source.Commit)
	if source.Remote == "" || source.Commit == "" || strings.ContainsAny(source.Remote+source.Commit, "\r\n\x00") {
		return Source{}, errors.New("skill-pack source requires nonempty remote and commit provenance")
	}
	if !commitPattern.MatchString(source.Commit) {
		return Source{}, errors.New("skill-pack source commit must be a full lowercase Git object ID")
	}
	if source.ManifestPath == "" {
		source.ManifestPath = DefaultManifest
	}
	clean, err := cleanRelativePath(source.ManifestPath, "manifest path")
	if err != nil {
		return Source{}, err
	}
	source.ManifestPath = clean
	return source, nil
}

func validateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: version %d", ErrUnsupportedSchema, manifest.SchemaVersion)
	}
	if len(manifest.Skills) > 128 {
		return errors.New("skill-pack contains more than 128 skills")
	}
	seen := map[string]bool{}
	for i := range manifest.Skills {
		skill := &manifest.Skills[i]
		if !skillNamePattern.MatchString(skill.Name) {
			return fmt.Errorf("skill name %q must be a lowercase hyphenated slug", skill.Name)
		}
		if seen[skill.Name] {
			return fmt.Errorf("duplicate skill name %q", skill.Name)
		}
		seen[skill.Name] = true
		clean, err := cleanRelativePath(skill.Path, "skill path")
		if err != nil {
			return fmt.Errorf("skill %q: %w", skill.Name, err)
		}
		skill.Path = clean
		if len(skill.Targets) == 0 {
			return fmt.Errorf("skill %q requires at least one target", skill.Name)
		}
		targetSeen := map[string]bool{}
		for _, target := range skill.Targets {
			switch target {
			case "claude", "codex", "cursor", "hermes", "omp", "multica":
			default:
				return fmt.Errorf("skill %q has unsupported target %q", skill.Name, target)
			}
			if targetSeen[target] {
				return fmt.Errorf("skill %q repeats target %q", skill.Name, target)
			}
			targetSeen[target] = true
		}
		skill.Targets = uniqueSorted(skill.Targets)
	}
	return nil
}

func readSkillTree(repositoryRoot, relative string) ([]fileAsset, string, error) {
	root := filepath.Join(repositoryRoot, filepath.FromSlash(relative))
	if err := validateContainedPath(repositoryRoot, root); err != nil {
		return nil, "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, "", errors.New("source skill must be a regular directory")
	}
	assets := []fileAsset{}
	total := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source skill contains symlink: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source skill contains non-regular file: %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == MarkerName {
			return fmt.Errorf("source skill reserves %s for agentctl provenance", MarkerName)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		total += len(data)
		if total > maxSkillBytes || len(assets) >= maxSkillFiles {
			return errors.New("source skill exceeds file-count or byte limit")
		}
		assets = append(assets, fileAsset{Path: rel, Mode: info.Mode().Perm(), Data: data})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if !containsAsset(assets, "SKILL.md") {
		return nil, "", errors.New("source skill is missing SKILL.md")
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Path < assets[j].Path })
	return assets, digestAssets(assets), nil
}

func inspect(destination string, skill preparedSkill, harnesses []string, source Source, manifestDigest string) Action {
	action := Action{Skill: skill.Name, Harnesses: harnesses, Destination: destination, State: "current", Digest: skill.Digest}
	info, err := os.Lstat(destination)
	if os.IsNotExist(err) {
		action.State, action.Changed = "install", true
		return action
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		action.State, action.Reason = "conflict", "destination is not a regular managed directory"
		return action
	}
	markerValue, err := loadMarker(filepath.Join(destination, MarkerName))
	if err != nil || markerValue.ManagedBy != "agentctl" || markerValue.Skill != skill.Name {
		action.State, action.Reason = "conflict", "existing skill has no valid agentctl managed marker"
		return action
	}
	assets, installedDigest, err := readInstalledTree(destination)
	_ = assets
	if err != nil || installedDigest != markerValue.ContentSHA256 {
		action.State, action.Reason = "drifted", "installed skill content differs from its managed marker"
		return action
	}
	wantHarnesses := uniqueSorted(harnesses)
	if installedDigest != skill.Digest {
		action.State, action.Changed = "upgrade", true
		return action
	}
	if markerValue.SourceRemote != source.Remote || markerValue.SourceCommit != source.Commit || markerValue.ManifestSHA256 != manifestDigest || strings.Join(uniqueSorted(markerValue.Harnesses), ",") != strings.Join(wantHarnesses, ",") {
		action.State, action.Changed = "metadata_update", true
	}
	return action
}

func writeSkill(destination string, skill preparedSkill, harnesses []string, source Source, manifestDigest string) error {
	root := filepath.Dir(destination)
	if err := validateNoSymlinkComponents(root); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := validateNoSymlinkComponents(root); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, ".agentctl-skill-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, asset := range skill.Assets {
		path := filepath.Join(stage, filepath.FromSlash(asset.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		mode := fs.FileMode(0o600)
		if asset.Mode&0o100 != 0 {
			mode = 0o700
		}
		if err := os.WriteFile(path, asset.Data, mode); err != nil {
			return err
		}
	}
	metadata := marker{SchemaVersion: SchemaVersion, ManagedBy: "agentctl", Skill: skill.Name, Harnesses: uniqueSorted(harnesses), SourceRemote: source.Remote, SourceCommit: source.Commit, ManifestSHA256: manifestDigest, ContentSHA256: skill.Digest}
	markerBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, MarkerName), append(markerBytes, '\n'), 0o600); err != nil {
		return err
	}
	backup := ""
	if _, err := os.Lstat(destination); err == nil {
		backup, err = os.MkdirTemp(root, ".agentctl-skill-backup-")
		if err != nil {
			return err
		}
		if err := os.Remove(backup); err != nil {
			return err
		}
		if err := os.Rename(destination, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		if backup != "" {
			_ = os.Rename(backup, destination)
		}
		return err
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return err
		}
	}
	return nil
}

func readInstalledTree(root string) ([]fileAsset, string, error) {
	assets := []fileAsset{}
	total := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("installed skill contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("installed skill contains a non-regular file")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == MarkerName {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		total += len(data)
		if total > maxSkillBytes || len(assets) >= maxSkillFiles {
			return errors.New("installed skill exceeds file-count or byte limit")
		}
		assets = append(assets, fileAsset{Path: rel, Mode: info.Mode().Perm(), Data: data})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Path < assets[j].Path })
	return assets, digestAssets(assets), nil
}

func digestAssets(assets []fileAsset) string {
	hash := sha256.New()
	for _, asset := range assets {
		_, _ = fmt.Fprintf(hash, "%s\x00%04o\x00%d\x00", asset.Path, normalizedMode(asset.Mode), len(asset.Data))
		_, _ = hash.Write(asset.Data)
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func normalizedMode(mode fs.FileMode) fs.FileMode {
	if mode&0o100 != 0 {
		return 0o700
	}
	return 0o600
}

func loadMarker(path string) (marker, error) {
	data, err := readRegularBounded(path, maxManifestBytes)
	if err != nil {
		return marker{}, err
	}
	var value marker
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return marker{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return marker{}, errors.New("managed marker has trailing content")
	}
	if value.SchemaVersion != SchemaVersion || value.ManagedBy != "agentctl" || value.Skill == "" || value.ContentSHA256 == "" {
		return marker{}, errors.New("managed marker is invalid")
	}
	return value, nil
}

func readRegularBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular non-symlink file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d-byte limit", limit)
	}
	return os.ReadFile(path)
}

func cleanRelativePath(value, label string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00\\") || filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be a repository-relative path", label)
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return "", fmt.Errorf("%s must stay outside .git and inside the repository", label)
	}
	return clean, nil
}

func cleanExistingDirectory(value, label string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	abs, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%s must be a regular directory", label)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

type reconcileLock struct{ file *os.File }

func acquireLock(home string) (*reconcileLock, error) {
	home, err := cleanExistingDirectory(home, "home")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	directory := filepath.Join(home, ".local", "state", "agentctl")
	if err := validateNoSymlinkComponents(directory); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "skills.lock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: skills lock is unmanaged", ErrConflict)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: another skills reconcile is active", ErrConflict)
		}
		return nil, err
	}
	return &reconcileLock{file: file}, nil
}

func (lock *reconcileLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}

func validateContainedPath(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("skill path escapes repository root")
	}
	return validateNoSymlinkComponents(path)
}

func validateNoSymlinkComponents(path string) error {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(path, current)
	for _, part := range strings.Split(remainder, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path contains symlink component: %s", current)
		}
	}
	return nil
}

func harnessRoot(home, harness string) string {
	switch harness {
	case "claude":
		return filepath.Join(home, ".claude", "skills")
	case "codex", "omp":
		return filepath.Join(home, ".agents", "skills")
	case "cursor":
		return filepath.Join(home, ".cursor", "skills")
	case "hermes":
		return filepath.Join(home, ".hermes", "skills")
	default:
		return ""
	}
}

func containsAsset(assets []fileAsset, path string) bool {
	for _, asset := range assets {
		if asset.Path == path {
			return true
		}
	}
	return false
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func stringSet(values []string) map[string]bool {
	result := map[string]bool{}
	for _, value := range uniqueSorted(values) {
		result[value] = true
	}
	return result
}

func sortedKeys(values map[string][]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
