package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/portableasset"
)

const minimumPortableVersion = "v0.1.4"
const bootstrapManagedMarkerName = "agentctl-managed.json"

type bootstrapSkillEntry struct {
	Path     string `json:"path"`
	Digest   string `json:"digest"`
	Revision string `json:"revision,omitempty"`
}

type bootstrapHarness struct {
	Name      string                `json:"name"`
	State     string                `json:"state"`
	Canonical string                `json:"canonical"`
	Detected  bool                  `json:"detected,omitempty"`
	Entries   []bootstrapSkillEntry `json:"entries"`
}

type bootstrapBinary struct {
	Path           string `json:"path,omitempty"`
	Resolution     string `json:"resolution"`
	Version        string `json:"version"`
	MinimumVersion string `json:"minimum_version"`
	Compatible     bool   `json:"compatible"`
}

type bootstrapStatus struct {
	Healthy          bool               `json:"healthy"`
	Home             string             `json:"home"`
	Binary           bootstrapBinary    `json:"binary"`
	Harnesses        []bootstrapHarness `json:"harnesses"`
	ConfigPresent    bool               `json:"config_present"`
	SupervisorSocket bool               `json:"supervisor_socket"`
	SkillDigest      string             `json:"skill_digest,omitempty"`
	SkillRevision    string             `json:"skill_revision,omitempty"`
	Problems         []string           `json:"problems"`
}

type bootstrapAction struct {
	Harnesses []string `json:"harnesses"`
	State     string   `json:"state"`
	Canonical string   `json:"canonical"`
	Reason    string   `json:"reason,omitempty"`
	Changed   bool     `json:"changed"`
}

type bootstrapUpdateResult struct {
	Home        string            `json:"home"`
	Revision    string            `json:"revision"`
	SkillDigest string            `json:"skill_digest"`
	Detected    []string          `json:"detected_harnesses"`
	Actions     []bootstrapAction `json:"actions"`
	Healthy     bool              `json:"healthy"`
	DryRun      bool              `json:"dry_run"`
}

type bootstrapManifest struct {
	SchemaVersion  int    `json:"schema_version"`
	DistributionID string `json:"distribution_id"`
	Revision       string `json:"revision"`
	Assets         []struct {
		ID   string `json:"id"`
		Hash string `json:"sha256"`
	} `json:"assets"`
}

type bootstrapManagedMarker struct {
	SchemaVersion  int      `json:"schema_version"`
	ManagedBy      string   `json:"managed_by"`
	DistributionID string   `json:"distribution_id"`
	Harnesses      []string `json:"harnesses"`
	Revision       string   `json:"revision"`
	SkillDigest    string   `json:"skill_digest"`
	ManifestDigest string   `json:"manifest_digest"`
}

type bootstrapHarnessSpec struct {
	Name             string
	Executables      []string
	Canonical        func(string) string
	Compatibility    []func(string) string
	ConfigCandidates func(string) []string
}

var bootstrapHarnessSpecs = []bootstrapHarnessSpec{
	{Name: "claude", Executables: []string{"claude"}, Canonical: func(home string) string { return filepath.Join(home, ".claude", "skills") }, ConfigCandidates: configCandidates(".claude", "settings.json", "config.json")},
	{Name: "codex", Executables: []string{"codex"}, Canonical: func(home string) string { return filepath.Join(home, ".agents", "skills") }, Compatibility: []func(string) string{func(home string) string { return filepath.Join(home, ".codex", "skills") }}, ConfigCandidates: configCandidates(".codex", "config.toml", "config.json")},
	{Name: "cursor", Executables: []string{"cursor-agent"}, Canonical: func(home string) string { return filepath.Join(home, ".cursor", "skills") }, ConfigCandidates: configCandidates(".cursor", "config.json", "settings.json")},
	{Name: "hermes", Executables: []string{"hermes"}, Canonical: func(home string) string { return filepath.Join(home, ".hermes", "skills") }, ConfigCandidates: configCandidates(".hermes", "config.json", "config.yaml", "config.yml")},
	{Name: "omp", Executables: []string{"omp"}, Canonical: func(home string) string { return filepath.Join(home, ".agents", "skills") }, Compatibility: []func(string) string{func(home string) string { return filepath.Join(home, ".omp", "agent", "skills") }}, ConfigCandidates: configCandidates(".omp", "config.json", "config.yaml", "config.yml")},
	// Multica skills are workspace/server scoped.  There is intentionally no
	// guessed local root; callers must supply an explicit target directory.
	{Name: "multica", Executables: []string{"multica"}, ConfigCandidates: configCandidates(".multica", "config.json")},
}

func configCandidates(dir string, names ...string) func(string) []string {
	return func(home string) []string {
		result := make([]string, 0, len(names))
		for _, name := range names {
			result = append(result, filepath.Join(home, dir, name))
		}
		return result
	}
}

func (a *app) bootstrapCommand(renderer output.Renderer, args []string) *output.Error {
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "usage: agentctl bootstrap status|update [flags]", false)
	}
	command := args[0]
	home, expectedValue, harnessValue, targetDir := "", "", "", ""
	dryRun := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--home", "--expected", "--harness", "--target-dir":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, args[i]+" requires a value", false)
			}
			i++
			switch args[i-1] {
			case "--home":
				home = args[i]
			case "--expected":
				expectedValue = args[i]
			case "--harness":
				harnessValue = args[i]
			case "--target-dir":
				targetDir = args[i]
			}
		case "--dry-run":
			dryRun = true
		default:
			return output.NewError(output.CodeUsage, "unknown bootstrap flag", false).WithDetail("flag", args[i])
		}
	}
	if command != "status" && command != "update" {
		return output.NewError(output.CodeUsage, "usage: agentctl bootstrap status|update [flags]", false)
	}
	if command == "status" && dryRun {
		return output.NewError(output.CodeUsage, "--dry-run is only valid for bootstrap update", false)
	}
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return output.Wrap(output.CodeDependencyUnavailable, "resolve home directory", false, err)
		}
	}
	if err := validateBootstrapHome(home); err != nil {
		return output.Wrap(output.CodeUsage, "invalid bootstrap home", false, err)
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = filepath.Clean(resolved)
	}
	if targetDir != "" {
		if err := validateBootstrapPath(targetDir); err != nil {
			return output.Wrap(output.CodeUsage, "invalid bootstrap target directory", false, err)
		}
	}
	selected, err := parseBootstrapHarnesses(home, harnessValue, expectedValue, a.getenv)
	if err != nil {
		return err
	}
	if targetDir != "" && len(selected) != 1 {
		return output.NewError(output.CodeUsage, "--target-dir requires exactly one selected harness", false)
	}
	if command == "status" {
		status := buildBootstrapStatusAt(home, selected, a.getenv, targetDir)
		return renderBootstrapStatus(renderer, status)
	}
	return a.bootstrapUpdate(renderer, home, selected, targetDir, dryRun)
}

func renderBootstrapStatus(renderer output.Renderer, status bootstrapStatus) *output.Error {
	lines := []output.Line{{Lead: "bootstrap", Fields: []output.Field{{Name: "healthy", Value: status.Healthy}, {Name: "binary", Value: status.Binary.Path}, {Name: "version", Value: status.Binary.Version}, {Name: "skill_digest", Value: status.SkillDigest}}}}
	for _, harness := range status.Harnesses {
		lines = append(lines, output.Line{Lead: "harness", Fields: []output.Field{{Name: "name", Value: harness.Name}, {Name: "state", Value: harness.State}, {Name: "canonical", Value: harness.Canonical}, {Name: "detected", Value: harness.Detected}, {Name: "registrations", Value: len(harness.Entries)}}})
	}
	if err := renderer.Success(output.Success{Result: status, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func parseExpectedHarnesses(home, value string) ([]string, *output.Error) {
	return parseBootstrapHarnesses(home, "", value, nil)
}

func parseBootstrapHarnesses(home, harnessValue, expectedValue string, getenv func(string) string) ([]string, *output.Error) {
	if strings.TrimSpace(harnessValue) != "" && strings.TrimSpace(expectedValue) != "" {
		return nil, output.NewError(output.CodeUsage, "--harness and --expected cannot be combined", false)
	}
	value := harnessValue
	if value == "" {
		value = expectedValue
	}
	if strings.TrimSpace(value) == "" {
		return detectBootstrapHarnesses(home, getenv), nil
	}
	allowed := map[string]bool{"codex": true, "omp": true, "hermes": true, "claude": true, "cursor": true, "multica": true}
	seen := map[string]bool{}
	for _, raw := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if !allowed[name] {
			return nil, output.NewError(output.CodeUsage, "unsupported bootstrap harness", false).WithDetail("harness", name)
		}
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func detectBootstrapHarnesses(home string, getenv func(string) string) []string {
	result := make([]string, 0, len(bootstrapHarnessSpecs))
	for _, spec := range bootstrapHarnessSpecs {
		found := false
		for _, executable := range spec.Executables {
			if path := bootstrapLookPath(executable, getenv); path != "" {
				found = true
				break
			}
		}
		if !found && spec.ConfigCandidates != nil {
			for _, candidate := range spec.ConfigCandidates(home) {
				if regularFile(candidate) {
					found = true
					break
				}
			}
		}
		if found && spec.Name != "multica" { // no local Multica root to mutate.
			result = append(result, spec.Name)
		}
	}
	sort.Strings(result)
	return result
}

func bootstrapLookPath(name string, getenv func(string) string) string {
	if filepath.IsAbs(name) {
		if executableFile(name) {
			return name
		}
		return ""
	}
	pathValue := os.Getenv("PATH")
	if getenv != nil {
		// The injected environment is authoritative.  Tests and callers with a
		// deliberately empty PATH must not accidentally discover the parent
		// process's binaries.
		pathValue = getenv("PATH")
	}
	if pathValue == "" {
		return ""
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		if executableFile(candidate) {
			return candidate
		}
	}
	return ""
}

func buildBootstrapStatus(home string, expected []string, getenv func(string) string) bootstrapStatus {
	return buildBootstrapStatusAt(home, expected, getenv, "")
}

func buildBootstrapStatusAt(home string, expected []string, getenv func(string) string, targetOverride string) bootstrapStatus {
	status := bootstrapStatus{Healthy: true, Home: home, Binary: resolveAgentctlBinary(home, getenv), Harnesses: []bootstrapHarness{}, Problems: []string{}}
	embeddedSkill, embeddedErr := portableasset.Skill()
	exactReleaseRevision := "tree:v" + strings.TrimPrefix(strings.SplitN(version, "-", 2)[0], "v")
	compareEmbeddedRelease := embeddedErr == nil && embeddedSkill.Revision == exactReleaseRevision
	status.ConfigPresent = regularFile(filepath.Join(home, ".config", "agentctl", "config.json"))
	status.SupervisorSocket = socketFile(filepath.Join(home, ".local", "state", "agentctl", "supervisor.sock"))
	if status.Binary.Path == "" {
		status.Healthy = false
		status.Problems = append(status.Problems, "binary_missing")
	} else if !status.Binary.Compatible {
		status.Healthy = false
		status.Problems = append(status.Problems, "binary_too_old")
	}
	digests := map[string]bool{}
	revisions := map[string]bool{}
	for _, name := range expected {
		spec := bootstrapSpec(name)
		if spec == nil {
			continue
		}
		canonical := ""
		if spec.Canonical != nil {
			canonical = spec.Canonical(home)
		}
		if targetOverride != "" {
			canonical = targetOverride
		}
		if canonical == "" {
			status.Healthy = false
			status.Problems = append(status.Problems, name+"_root_unavailable")
			status.Harnesses = append(status.Harnesses, bootstrapHarness{Name: name, State: "unsupported", Canonical: "", Detected: bootstrapContains(expected, name), Entries: []bootstrapSkillEntry{}})
			continue
		}
		roots := []string{canonical}
		if targetOverride == "" {
			for _, compatibility := range spec.Compatibility {
				roots = append(roots, compatibility(home))
			}
		}
		entries := skillEntries(roots)
		state := "installed"
		inspection := inspectBootstrapStatusRoot(canonical)
		if inspection.State == "missing" {
			state = "missing"
			status.Healthy = false
			status.Problems = append(status.Problems, name+"_missing")
		} else if inspection.State == "conflict" {
			state = "conflict"
			status.Healthy = false
			status.Problems = append(status.Problems, name+"_conflict")
		} else if inspection.State == "drifted" {
			state = "drifted"
			status.Healthy = false
			status.Problems = append(status.Problems, name+"_drifted")
		} else if len(entries) > 1 {
			state = "duplicate"
			status.Healthy = false
			status.Problems = append(status.Problems, name+"_duplicate")
		}
		for _, entry := range entries {
			digests[entry.Digest] = true
			if entry.Revision != "" {
				revisions[entry.Revision] = true
			}
		}
		if compareEmbeddedRelease {
			for _, entry := range entries {
				if entry.Digest != embeddedSkill.Digest || entry.Revision != embeddedSkill.Revision {
					if state == "installed" {
						state = "outdated"
					}
					status.Healthy = false
					status.Problems = append(status.Problems, name+"_outdated")
				}
			}
		}
		status.Harnesses = append(status.Harnesses, bootstrapHarness{Name: name, State: state, Canonical: canonical, Detected: bootstrapContains(expected, name), Entries: entries})
	}
	if len(digests) == 1 {
		for digest := range digests {
			status.SkillDigest = digest
		}
	} else if len(digests) > 1 {
		status.Healthy = false
		status.Problems = append(status.Problems, "skill_digest_drift")
	}
	if len(revisions) == 1 {
		for revision := range revisions {
			status.SkillRevision = revision
		}
		if !versionAtLeast(strings.TrimPrefix(status.SkillRevision, "tree:"), minimumPortableVersion) {
			status.Healthy = false
			status.Problems = append(status.Problems, "skill_too_old")
		}
	} else if len(revisions) > 1 {
		status.Healthy = false
		status.Problems = append(status.Problems, "skill_revision_drift")
	}
	sort.Slice(status.Harnesses, func(i, j int) bool { return status.Harnesses[i].Name < status.Harnesses[j].Name })
	sort.Strings(status.Problems)
	return status
}

// Status remains tolerant of the compact v0.1.6 test/install manifest.  The
// mutating update path below is stricter and requires a distribution binding;
// status should still report provenance for older manifests rather than
// turning every pre-marker installation into an opaque conflict.
func inspectBootstrapStatusRoot(root string) bootstrapRootInspection {
	inspection := bootstrapRootInspection{Root: root, State: "missing"}
	if err := validateBootstrapPath(root); err != nil {
		if os.IsNotExist(err) {
			return inspection
		}
		inspection.State = "conflict"
		return inspection
	}
	managed := filepath.Join(root, "agentctl-portable")
	skillPath := filepath.Join(managed, "SKILL.md")
	manifestPath := filepath.Join(managed, "revision-manifest.json")
	skillInfo, err := os.Lstat(skillPath)
	if err != nil {
		if os.IsNotExist(err) {
			return inspection
		}
		inspection.State = "conflict"
		return inspection
	}
	if skillInfo.Mode()&os.ModeSymlink != 0 || !skillInfo.Mode().IsRegular() {
		inspection.State = "conflict"
		return inspection
	}
	data, err := os.ReadFile(skillPath)
	if err != nil {
		inspection.State = "conflict"
		return inspection
	}
	sum := sha256.Sum256(data)
	inspection.Digest = "sha256:" + hex.EncodeToString(sum[:])
	if manifestData, err := os.ReadFile(manifestPath); err == nil {
		var manifest bootstrapManifest
		if json.Unmarshal(manifestData, &manifest) == nil {
			inspection.Revision = manifest.Revision
		}
	}
	inspection.State = "managed"
	return inspection
}

type bootstrapRootInspection struct {
	Root     string
	State    string
	Digest   string
	Revision string
}

func inspectBootstrapRoot(root, expectedDigest string) bootstrapRootInspection {
	inspection := bootstrapRootInspection{Root: root, State: "missing"}
	if err := validateBootstrapPath(root); err != nil {
		if os.IsNotExist(err) {
			return inspection
		}
		inspection.State = "conflict"
		return inspection
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return inspection
		}
		inspection.State = "conflict"
		return inspection
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		inspection.State = "conflict"
		return inspection
	}
	managed := filepath.Join(root, "agentctl-portable")
	managedInfo, err := os.Lstat(managed)
	if err != nil {
		if os.IsNotExist(err) {
			return inspection
		}
		inspection.State = "conflict"
		return inspection
	}
	if managedInfo.Mode()&os.ModeSymlink != 0 || !managedInfo.IsDir() {
		inspection.State = "conflict"
		return inspection
	}
	skillPath := filepath.Join(managed, "SKILL.md")
	manifestPath := filepath.Join(managed, "revision-manifest.json")
	skillInfo, skillErr := os.Lstat(skillPath)
	manifestInfo, manifestErr := os.Lstat(manifestPath)
	if skillErr != nil {
		if os.IsNotExist(skillErr) && os.IsNotExist(manifestErr) {
			return inspection
		}
		inspection.State = "conflict"
		return inspection
	}
	if manifestErr != nil {
		inspection.State = "conflict"
		return inspection
	}
	if skillInfo.Mode()&os.ModeSymlink != 0 || !skillInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
		inspection.State = "conflict"
		return inspection
	}
	skill, err := os.ReadFile(skillPath)
	if err != nil {
		inspection.State = "conflict"
		return inspection
	}
	sum := sha256.Sum256(skill)
	inspection.Digest = "sha256:" + hex.EncodeToString(sum[:])
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		inspection.State = "conflict"
		return inspection
	}
	var metadata bootstrapManifest
	if err := json.Unmarshal(manifest, &metadata); err != nil || metadata.DistributionID != "agentctl-portable" {
		inspection.State = "conflict"
		return inspection
	}
	inspection.Revision = metadata.Revision
	boundHash := ""
	for _, asset := range metadata.Assets {
		if asset.ID == "portable-skill" {
			boundHash = asset.Hash
			break
		}
	}
	if boundHash == "" || boundHash != strings.TrimPrefix(inspection.Digest, "sha256:") {
		inspection.State = "drifted"
		return inspection
	}
	if expectedDigest != "" && inspection.Digest != expectedDigest {
		inspection.State = "drifted"
		return inspection
	}
	inspection.State = "managed"
	return inspection
}

func (a *app) bootstrapUpdate(renderer output.Renderer, home string, selected []string, targetOverride string, dryRun bool) *output.Error {
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = filepath.Clean(resolved)
	}
	skill, err := portableasset.Skill()
	if err != nil {
		return output.Wrap(output.CodeInternal, "load embedded portable skill", false, err)
	}
	manifest, err := portableasset.Manifest()
	if err != nil {
		return output.Wrap(output.CodeInternal, "load embedded distribution manifest", false, err)
	}
	manifestSum := sha256.Sum256(manifest)
	manifestDigest := "sha256:" + hex.EncodeToString(manifestSum[:])
	result := bootstrapUpdateResult{Home: home, Revision: skill.Revision, SkillDigest: skill.Digest, Detected: append([]string(nil), selected...), Actions: []bootstrapAction{}, Healthy: true, DryRun: dryRun}
	filtered := make([]string, 0, len(selected))
	for _, name := range selected {
		if name == "multica" {
			result.Actions = append(result.Actions, bootstrapAction{Harnesses: []string{"multica"}, State: "unsupported", Reason: "Multica bootstrap requires its runtime bundle installer; use distributions/install.sh with an explicit target directory", Changed: false})
			continue
		}
		filtered = append(filtered, name)
	}
	if len(filtered) == 0 {
		if bootstrapContains(selected, "multica") {
			return output.NewError(output.CodeCapabilityUnavailable, "Multica bootstrap requires its runtime bundle installer; use distributions/install.sh with an explicit target directory", false).WithDetail("harness", "multica")
		}
	}
	selected = filtered
	groups := map[string][]string{}
	for _, name := range selected {
		spec := bootstrapSpec(name)
		if spec == nil {
			result.Healthy = false
			result.Actions = append(result.Actions, bootstrapAction{Harnesses: []string{name}, State: "conflict", Reason: "unsupported harness", Changed: false})
			continue
		}
		root := ""
		if targetOverride != "" {
			root = targetOverride
		} else if spec.Canonical != nil {
			root = spec.Canonical(home)
		} else {
			result.Healthy = false
			result.Actions = append(result.Actions, bootstrapAction{Harnesses: []string{name}, State: "conflict", Reason: "no supported local root; pass --target-dir", Changed: false})
			continue
		}
		groups[root] = append(groups[root], name)
	}
	roots := make([]string, 0, len(groups))
	for root := range groups {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for _, root := range roots {
		names := groups[root]
		inspection := inspectBootstrapRoot(root, "")
		action := bootstrapAction{Harnesses: append([]string(nil), names...), Canonical: root, State: "noop", Changed: false}
		if inspection.State == "conflict" || inspection.State == "drifted" {
			result.Healthy = false
			action.State = inspection.State
			action.Reason = "existing managed path is not hash-bound and unmodified"
			result.Actions = append(result.Actions, action)
			continue
		}
		markerPresent, markerIntegrityValid := managedMarkerIntegrity(root)
		if markerPresent && !markerIntegrityValid {
			result.Healthy = false
			action.State = "conflict"
			action.Reason = "managed marker is malformed or does not match installed assets"
		} else if inspection.State == "missing" {
			action.State = "install"
			action.Changed = true
		} else if inspection.Digest != skill.Digest {
			action.State = "upgrade"
			action.Changed = true
		} else if !managedMarkerValid(root, names, skill.Digest, manifestDigest) {
			action.State = "adopt"
			action.Changed = true
		}
		if targetOverride == "" {
			for _, name := range names {
				spec := bootstrapSpec(name)
				if spec == nil {
					continue
				}
				for _, compatibility := range spec.Compatibility {
					legacy := inspectBootstrapRoot(compatibility(home), "")
					switch legacy.State {
					case "conflict", "drifted":
						result.Healthy = false
						action.State = legacy.State
						action.Reason = "compatibility root is conflicting or drifted"
					case "managed":
						result.Healthy = false
						action.State = "duplicate"
						action.Reason = "managed compatibility copy exists; deletion is explicit"
					}
				}
			}
		}
		result.Actions = append(result.Actions, action)
	}
	for _, action := range result.Actions {
		if action.State == "conflict" || action.State == "drifted" || action.State == "duplicate" {
			return output.NewError(output.CodeConflict, "bootstrap update refused a conflicting or drifted managed path", false).WithDetail("actions", result.Actions).WithDetail("home", home)
		}
	}
	if !dryRun {
		for _, action := range result.Actions {
			if !action.Changed {
				continue
			}
			if err := writeBootstrapRoot(action.Canonical, action.Harnesses, skill.Bytes, manifest, skill.Digest, manifestDigest); err != nil {
				return output.Wrap(output.CodeConflict, "write portable skill", false, err).WithDetail("canonical", action.Canonical)
			}
		}
	}
	lines := []output.Line{{Lead: "bootstrap.update", Fields: []output.Field{{Name: "healthy", Value: result.Healthy}, {Name: "dry_run", Value: dryRun}, {Name: "revision", Value: result.Revision}, {Name: "skill_digest", Value: result.SkillDigest}}}}
	for _, action := range result.Actions {
		lines = append(lines, output.Line{Lead: "action", Fields: []output.Field{{Name: "harnesses", Value: action.Harnesses}, {Name: "state", Value: action.State}, {Name: "canonical", Value: action.Canonical}, {Name: "changed", Value: action.Changed}}})
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func managedMarkerValid(root string, harnesses []string, digest, manifestDigest string) bool {
	path := filepath.Join(root, "agentctl-portable", bootstrapManagedMarkerName)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var marker bootstrapManagedMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return false
	}
	if marker.SchemaVersion != 1 || marker.ManagedBy != "agentctl" || marker.DistributionID != "agentctl-portable" || marker.SkillDigest != digest || marker.ManifestDigest != manifestDigest {
		return false
	}
	sort.Strings(marker.Harnesses)
	want := append([]string(nil), harnesses...)
	sort.Strings(want)
	return strings.Join(marker.Harnesses, ",") == strings.Join(want, ",")
}

func managedMarkerIntegrity(root string) (bool, bool) {
	path := filepath.Join(root, "agentctl-portable", bootstrapManagedMarkerName)
	info, err := os.Lstat(path)
	if err != nil {
		return !os.IsNotExist(err), false
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return true, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true, false
	}
	var marker bootstrapManagedMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return true, false
	}
	skill, err := os.ReadFile(filepath.Join(root, "agentctl-portable", "SKILL.md"))
	if err != nil {
		return true, false
	}
	manifest, err := os.ReadFile(filepath.Join(root, "agentctl-portable", "revision-manifest.json"))
	if err != nil {
		return true, false
	}
	skillSum := sha256.Sum256(skill)
	manifestSum := sha256.Sum256(manifest)
	digest := "sha256:" + hex.EncodeToString(skillSum[:])
	manifestDigest := "sha256:" + hex.EncodeToString(manifestSum[:])
	valid := marker.SchemaVersion == 1 && marker.ManagedBy == "agentctl" && marker.DistributionID == "agentctl-portable" && marker.SkillDigest == digest && marker.ManifestDigest == manifestDigest
	return true, valid
}

func writeBootstrapRoot(root string, harnesses []string, skill, manifest []byte, skillDigest, manifestDigest string) error {
	if err := validateBootstrapPath(root); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := validateBootstrapPath(root); err != nil {
		return err
	}
	managed := filepath.Join(root, "agentctl-portable")
	if info, err := os.Lstat(managed); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("managed path is not a directory: %s", managed)
		}
	} else if os.IsNotExist(err) {
		if err := os.Mkdir(managed, 0o700); err != nil {
			return err
		}
	} else {
		return err
	}
	stage, err := os.MkdirTemp(managed, ".agentctl-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	marker := bootstrapManagedMarker{SchemaVersion: 1, ManagedBy: "agentctl", DistributionID: "agentctl-portable", Harnesses: append([]string(nil), harnesses...), Revision: manifestRevision(manifest), SkillDigest: skillDigest, ManifestDigest: manifestDigest}
	markerBytes, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	markerBytes = append(markerBytes, '\n')
	for name, data := range map[string][]byte{"SKILL.md": skill, "revision-manifest.json": manifest, bootstrapManagedMarkerName: markerBytes} {
		path := filepath.Join(stage, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
	}
	for _, name := range []string{"SKILL.md", "revision-manifest.json", bootstrapManagedMarkerName} {
		destination := filepath.Join(managed, name)
		if info, err := os.Lstat(destination); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("managed asset is not a regular file: %s", destination)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	// Each destination is validated before any rename.  Keep a private backup
	// until every replacement succeeds so a late filesystem error cannot leave
	// a partially upgraded managed directory.
	backup, err := os.MkdirTemp(managed, ".agentctl-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(backup)
	names := []string{"SKILL.md", "revision-manifest.json", bootstrapManagedMarkerName}
	backedUp := make([]string, 0, len(names))
	installed := make([]string, 0, len(names))
	rollback := func() {
		for _, name := range installed {
			destination := filepath.Join(managed, name)
			_ = os.Remove(destination)
		}
		for i := len(backedUp) - 1; i >= 0; i-- {
			name := backedUp[i]
			_ = os.Rename(filepath.Join(backup, name), filepath.Join(managed, name))
		}
	}
	for _, name := range names {
		destination := filepath.Join(managed, name)
		if _, err := os.Lstat(destination); err == nil {
			if err := os.Rename(destination, filepath.Join(backup, name)); err != nil {
				rollback()
				return err
			}
			backedUp = append(backedUp, name)
		}
		if err := os.Rename(filepath.Join(stage, name), destination); err != nil {
			rollback()
			return err
		}
		installed = append(installed, name)
	}
	return nil
}

func manifestRevision(data []byte) string {
	var metadata bootstrapManifest
	if json.Unmarshal(data, &metadata) == nil {
		return metadata.Revision
	}
	return ""
}

func skillEntries(roots []string) []bootstrapSkillEntry {
	entries := make([]bootstrapSkillEntry, 0, len(roots))
	seen := map[string]bool{}
	for _, root := range roots {
		path := filepath.Join(root, "agentctl-portable", "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		real := path
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			real = resolved
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		sum := sha256.Sum256(data)
		entry := bootstrapSkillEntry{Path: path, Digest: "sha256:" + hex.EncodeToString(sum[:])}
		manifestPath := filepath.Join(root, "agentctl-portable", "revision-manifest.json")
		if manifestData, err := os.ReadFile(manifestPath); err == nil {
			var manifest bootstrapManifest
			if json.Unmarshal(manifestData, &manifest) == nil {
				entry.Revision = manifest.Revision
			}
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}

func bootstrapSpec(name string) *bootstrapHarnessSpec {
	for i := range bootstrapHarnessSpecs {
		if bootstrapHarnessSpecs[i].Name == name {
			return &bootstrapHarnessSpecs[i]
		}
	}
	return nil
}

func bootstrapContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func resolveAgentctlBinary(home string, getenv func(string) string) bootstrapBinary {
	value := bootstrapBinary{Version: version, MinimumVersion: minimumPortableVersion, Compatible: versionAtLeast(version, minimumPortableVersion), Resolution: "missing"}
	if candidate, err := os.Executable(); err == nil && executableFile(candidate) {
		value.Path, value.Resolution = candidate, "self"
		return value
	}
	if getenv != nil {
		if candidate := strings.TrimSpace(getenv("AGENTCTL_BIN")); executableFile(candidate) {
			value.Path, value.Resolution = candidate, "environment"
			return value
		}
	}
	candidate := filepath.Join(home, ".local", "bin", "agentctl")
	if executableFile(candidate) {
		value.Path, value.Resolution = candidate, "canonical_fallback"
	}
	return value
}

func executableFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func socketFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func validateBootstrapHome(home string) error {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return errors.New("home must be an absolute clean path")
	}
	info, err := os.Lstat(home)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("home must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New("home is not a directory")
	}
	return nil
}

func validateBootstrapPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("path must be an absolute clean path")
	}
	current := string(filepath.Separator)
	remainder := strings.TrimPrefix(path, current)
	for remainder != "" {
		part := remainder
		if index := strings.IndexByte(remainder, filepath.Separator); index >= 0 {
			part = remainder[:index]
			remainder = remainder[index+1:]
		} else {
			remainder = ""
		}
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path contains symlink component: %s", current)
		}
	}
	return nil
}

func versionAtLeast(current, minimum string) bool {
	parse := func(value string) ([3]int, error) {
		var out [3]int
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		value = strings.SplitN(value, "-", 2)[0]
		_, err := fmtSscanfVersion(value, &out)
		return out, err
	}
	currentValue, currentErr := parse(current)
	minimumValue, minimumErr := parse(minimum)
	if currentErr != nil || minimumErr != nil {
		return false
	}
	for i := range currentValue {
		if currentValue[i] != minimumValue[i] {
			return currentValue[i] > minimumValue[i]
		}
	}
	return true
}

func fmtSscanfVersion(value string, out *[3]int) (int, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return 0, errors.New("version must contain three components")
	}
	for i, part := range parts {
		var parsed int
		if _, err := fmt.Sscanf(part, "%d", &parsed); err != nil {
			return i, err
		}
		out[i] = parsed
	}
	return 3, nil
}
