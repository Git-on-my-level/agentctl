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
)

const minimumPortableVersion = "v0.1.4"

type bootstrapSkillEntry struct {
	Path     string `json:"path"`
	Digest   string `json:"digest"`
	Revision string `json:"revision,omitempty"`
}

type bootstrapHarness struct {
	Name      string                `json:"name"`
	State     string                `json:"state"`
	Canonical string                `json:"canonical"`
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

type bootstrapManifest struct {
	Revision string `json:"revision"`
}

func (a *app) bootstrapCommand(renderer output.Renderer, args []string) *output.Error {
	if len(args) == 0 || args[0] != "status" {
		return output.NewError(output.CodeUsage, "usage: agentctl bootstrap status [--home path] [--expected harnesses]", false)
	}
	home := ""
	expectedValue := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--home":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--home requires a value", false)
			}
			i++
			home = args[i]
		case "--expected":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--expected requires a value", false)
			}
			i++
			expectedValue = args[i]
		default:
			return output.NewError(output.CodeUsage, "unknown bootstrap status flag", false).WithDetail("flag", args[i])
		}
	}
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return output.Wrap(output.CodeDependencyUnavailable, "resolve home directory", false, err)
		}
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return output.NewError(output.CodeUsage, "--home must be an absolute clean path", false)
	}
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() {
		return output.NewError(output.CodeNotFound, "home directory not found", false).WithDetail("home", home)
	}
	expected, problem := parseExpectedHarnesses(home, expectedValue)
	if problem != nil {
		return problem
	}
	status := buildBootstrapStatus(home, expected, a.getenv)
	lines := []output.Line{{Lead: "bootstrap", Fields: []output.Field{{Name: "healthy", Value: status.Healthy}, {Name: "binary", Value: status.Binary.Path}, {Name: "version", Value: status.Binary.Version}, {Name: "skill_digest", Value: status.SkillDigest}}}}
	for _, harness := range status.Harnesses {
		lines = append(lines, output.Line{Lead: "harness", Fields: []output.Field{{Name: "name", Value: harness.Name}, {Name: "state", Value: harness.State}, {Name: "canonical", Value: harness.Canonical}, {Name: "registrations", Value: len(harness.Entries)}}})
	}
	if err := renderer.Success(output.Success{Result: status, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func parseExpectedHarnesses(home, value string) ([]string, *output.Error) {
	allowed := map[string]bool{"codex": true, "omp": true, "hermes": true, "claude": true, "cursor": true}
	if strings.TrimSpace(value) == "" {
		out := make([]string, 0, len(allowed))
		for _, item := range []struct{ name, path string }{
			{"codex", filepath.Join(home, ".codex")},
			{"omp", filepath.Join(home, ".omp")},
			{"hermes", filepath.Join(home, ".hermes")},
			{"claude", filepath.Join(home, ".claude")},
			{"cursor", filepath.Join(home, ".cursor")},
		} {
			if _, err := os.Stat(item.path); err == nil {
				out = append(out, item.name)
			}
		}
		return out, nil
	}
	seen := map[string]bool{}
	for _, raw := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if !allowed[name] {
			return nil, output.NewError(output.CodeUsage, "unsupported expected harness", false).WithDetail("harness", name)
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

func buildBootstrapStatus(home string, expected []string, getenv func(string) string) bootstrapStatus {
	paths := map[string][]string{
		"codex":  {filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".codex", "skills")},
		"omp":    {filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".omp", "agent", "skills")},
		"hermes": {filepath.Join(home, ".hermes", "skills")},
		"claude": {filepath.Join(home, ".claude", "skills")},
		"cursor": {filepath.Join(home, ".cursor", "skills")},
	}
	canonical := map[string]string{
		"codex": filepath.Join(home, ".agents", "skills"), "omp": filepath.Join(home, ".agents", "skills"),
		"hermes": filepath.Join(home, ".hermes", "skills"), "claude": filepath.Join(home, ".claude", "skills"), "cursor": filepath.Join(home, ".cursor", "skills"),
	}
	status := bootstrapStatus{Healthy: true, Home: home, Binary: resolveAgentctlBinary(home, getenv), Harnesses: []bootstrapHarness{}, Problems: []string{}}
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
		entries := skillEntries(paths[name])
		state := "installed"
		if len(entries) == 0 {
			state = "missing"
			status.Healthy = false
			status.Problems = append(status.Problems, name+"_missing")
		} else {
			localDigests := map[string]bool{}
			for _, entry := range entries {
				localDigests[entry.Digest] = true
				digests[entry.Digest] = true
				if entry.Revision != "" {
					revisions[entry.Revision] = true
				}
			}
			if len(localDigests) > 1 {
				state = "drifted"
				status.Healthy = false
				status.Problems = append(status.Problems, name+"_drifted")
			} else if len(entries) > 1 {
				state = "duplicate"
				status.Healthy = false
				status.Problems = append(status.Problems, name+"_duplicate")
			} else if filepath.Dir(filepath.Dir(entries[0].Path)) != canonical[name] {
				state = "noncanonical"
				status.Healthy = false
				status.Problems = append(status.Problems, name+"_noncanonical")
			}
		}
		status.Harnesses = append(status.Harnesses, bootstrapHarness{Name: name, State: state, Canonical: canonical[name], Entries: entries})
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
