package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/portableasset"
)

// Reviewed published assets, never fetched or inferred from an installed file.
//
//go:embed bootstrap_releases.json
var bootstrapReleasedAssets []byte

type adoptionPlan struct {
	Path              string   `json:"path"`
	State             string   `json:"state"`
	Digest            string   `json:"digest"`
	PublishedReleases []string `json:"published_releases"`
	Backup            string   `json:"backup,omitempty"`
	DryRun            bool     `json:"dry_run"`
}

func adoptionSnapshot(path string) (map[string][]byte, []string, error) {
	if err := validateBootstrapPath(path); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !adoptionOwned(info) {
		return nil, nil, fmt.Errorf("legacy directory must be owner controlled")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, nil, err
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		name := entry.Name()
		if name != "SKILL.md" && name != "revision-manifest.json" && name != bootstrapManagedMarkerName {
			return nil, nil, fmt.Errorf("unrecognized legacy content: %s", name)
		}
		full := filepath.Join(path, name)
		info, err := os.Lstat(full)
		if err != nil || (!info.Mode().IsRegular() || !adoptionOwned(info) || info.Size() > 1<<20) {
			return nil, nil, fmt.Errorf("legacy asset must be a regular file: %s", name)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, nil, err
		}
		files[name] = data
	}
	var releases []struct {
		Release  string `json:"release"`
		Skill    string `json:"skill_digest"`
		Manifest string `json:"manifest_digest"`
	}
	if err := json.Unmarshal(bootstrapReleasedAssets, &releases); err != nil {
		return nil, nil, err
	}
	names := []string{}
	for _, release := range releases {
		if assetDigest(files["SKILL.md"]) != release.Skill {
			continue
		}
		if manifest, ok := files["revision-manifest.json"]; ok && assetDigest(manifest) != release.Manifest {
			continue
		}
		names = append(names, release.Release)
	}
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("skill and optional manifest do not match a published release")
	}
	if marker, ok := files[bootstrapManagedMarkerName]; ok {
		var value bootstrapManagedMarker
		decoder := json.NewDecoder(bytes.NewReader(marker))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.ManagedBy != "agentctl" || value.DistributionID != "agentctl-portable" || value.SchemaVersion != 1 || value.SkillDigest != assetDigest(files["SKILL.md"]) || value.ManifestDigest != assetDigest(files["revision-manifest.json"]) {
			return nil, nil, fmt.Errorf("legacy marker does not bind the published assets")
		}
	}
	return files, names, nil
}
func adoptionOwned(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Uid) == uint64(os.Geteuid()) && info.Mode().Perm()&0022 == 0
}
func assetDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func adoptionDigest(files map[string][]byte) string {
	data, _ := json.Marshal(files)
	return assetDigest(data)
}

// Adoption is intentionally one exact root per invocation, with no pointer,
// alias, config, or Multica workspace mutation. The plan digest binds all bytes.
func (a *app) bootstrapAdopt(renderer output.Renderer, args []string) *output.Error {
	home, harness, target, expected := "", "", "", ""
	apply := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--apply":
			apply = true
		case "--dry-run":
			return output.NewError(output.CodeUsage, "bootstrap adopt is plan-only by default; use --apply with --expected-digest to adopt", false)
		case "--home", "--harness", "--target-dir", "--expected-digest":
			flag := args[i]
			i++
			if i == len(args) {
				return output.NewError(output.CodeUsage, flag+" requires a value", false)
			}
			switch flag {
			case "--home":
				home = args[i]
			case "--harness":
				harness = args[i]
			case "--target-dir":
				target = args[i]
			case "--expected-digest":
				expected = args[i]
			}
		default:
			return output.NewError(output.CodeUsage, "unknown bootstrap adopt flag", false).WithDetail("flag", args[i])
		}
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if err := validateBootstrapHome(home); err != nil {
		return output.Wrap(output.CodeUsage, "invalid adoption home", false, err)
	}
	home, _ = filepath.EvalSymlinks(home)
	spec := bootstrapSpec(harness)
	if spec == nil || spec.Canonical == nil || strings.Contains(harness, ",") {
		return output.NewError(output.CodeUsage, "adopt requires one local --harness; Multica uses its runtime bundle installer", false)
	}
	if target == "" {
		target = spec.Canonical(home)
	}
	if err := validateBootstrapPath(target); err != nil {
		return output.Wrap(output.CodeUsage, "invalid adoption target", false, err)
	}
	backups := filepath.Join(home, ".local", "share", "agentctl", "adoption-backups")
	if relative, err := filepath.Rel(target, backups); err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return output.NewError(output.CodeConflict, "backup directory must be outside the selected skills root", false)
	}
	path := filepath.Join(target, "agentctl-portable")
	files, releases, err := adoptionSnapshot(path)
	if err != nil {
		return output.Wrap(output.CodeConflict, "legacy adoption refused", false, err)
	}
	plan := adoptionPlan{Path: path, State: "planned", Digest: adoptionDigest(files), PublishedReleases: releases, DryRun: !apply}
	if apply {
		if expected == "" || expected != plan.Digest {
			return output.NewError(output.CodeConflict, "adoption requires the current plan --expected-digest", false).WithDetail("digest", plan.Digest)
		}
		skill, err := portableasset.Skill()
		if err != nil {
			return output.Wrap(output.CodeInternal, "load portable skill", false, err)
		}
		manifest, err := portableasset.Manifest()
		if err != nil {
			return output.Wrap(output.CodeInternal, "load manifest", false, err)
		}

		if err := validateBootstrapPath(backups); err != nil {
			return output.Wrap(output.CodeConflict, "unsafe backup path", false, err)
		}
		if err := os.MkdirAll(backups, 0700); err != nil {
			return output.Wrap(output.CodeConflict, "create adoption backup directory", false, err)
		}
		backupInfo, err := os.Lstat(backups)
		if err != nil || !adoptionOwned(backupInfo) || backupInfo.Mode().Perm()&0077 != 0 {
			return output.NewError(output.CodeConflict, "adoption backup directory must be owner-only", false)
		}
		backup, err := os.MkdirTemp(backups, "adopt-")
		if err != nil {
			return output.Wrap(output.CodeConflict, "create adoption backup", false, err)
		}
		backup = filepath.Join(backup, "agentctl-portable")
		current, _, err := adoptionSnapshot(path)
		if err != nil || adoptionDigest(current) != plan.Digest {
			return output.NewError(output.CodeConflict, "legacy content changed after planning", true)
		}
		if err := os.Rename(path, backup); err != nil {
			return output.Wrap(output.CodeConflict, "backup legacy skill", false, err)
		}
		plan.Backup = backup
		moved, _, snapshotErr := adoptionSnapshot(backup)
		if snapshotErr != nil || adoptionDigest(moved) != plan.Digest {
			restoreErr := os.Rename(backup, path)
			return output.NewError(output.CodeConflict, "legacy content changed before backup completed", true).WithDetail("backup", backup).WithDetail("restored", restoreErr == nil)
		}
		if err := writeBootstrapRoot(target, []string{harness}, skill.Bytes, manifest, skill.Digest, assetDigest(manifest)); err != nil {
			// Only our new directory exists at path; preserve the original backup if
			// restoration cannot complete and return its exact recovery location.
			// Retain any partial replacement instead of deleting content
			// another owner-side operation could have created concurrently.
			restoreErr := os.Rename(path, filepath.Join(filepath.Dir(backup), "failed-install"))
			if os.IsNotExist(restoreErr) {
				restoreErr = nil
			}
			if restoreErr == nil {
				restoreErr = os.Rename(backup, path)
			}
			return output.Wrap(output.CodeConflict, "adoption failed", false, err).WithDetail("backup", backup).WithDetail("restored", restoreErr == nil)
		}
		plan.State = "adopted"
	}
	if err := renderer.Success(output.Success{Result: plan, Lines: []output.Line{{Lead: "adoption", Fields: []output.Field{{Name: "state", Value: plan.State}, {Name: "path", Value: path}, {Name: "digest", Value: plan.Digest}, {Name: "backup", Value: plan.Backup}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write adoption result", false, err)
	}
	return nil
}
