package knowledge

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// CheckScopedClean checks only files which the registration could ingest. Git
// status outside the allowlisted include globs is intentionally ignored.
// The returned digest is stable for a given scoped working-tree state and can
// be compared after ingestion to close the status/read race.
func CheckScopedClean(ctx context.Context, git GitRunner, reg SourceRegistration, checkoutDir string) (string, error) {
	if git == nil {
		return "", fmt.Errorf("nil git runner")
	}
	if err := rejectSymlinkComponents(checkoutDir, false); err != nil {
		return "", err
	}
	statusPath := reg.Subpath
	if statusPath == "" {
		statusPath = "."
	}
	out, err := git.Run(ctx, checkoutDir, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching", "--", statusPath)
	if err != nil {
		return "", fmt.Errorf("source working-tree status: %w", err)
	}
	entries, err := scopedStatusEntries(string(out), reg)
	if err != nil {
		return "", err
	}
	if len(entries) > 0 {
		return "", fmt.Errorf("included source files are dirty: %s", strings.Join(entries, ", "))
	}
	return digestBytes([]byte(strings.Join(entries, "\n"))), nil
}

func scopedStatusEntries(output string, reg SourceRegistration) ([]string, error) {
	entries := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 3 {
			return nil, fmt.Errorf("invalid porcelain status line")
		}
		status := line[:2]
		name := strings.TrimSpace(line[3:])
		if name == "" {
			return nil, fmt.Errorf("invalid porcelain status path")
		}
		// Porcelain v1 quotes paths containing whitespace/control bytes. Without
		// a full C-style unquoter, fail closed instead of accidentally treating a
		// dirty included path as excluded.
		if strings.HasPrefix(name, `"`) {
			return nil, fmt.Errorf("quoted porcelain status path cannot be safely classified")
		}
		paths := []string{name}
		// Porcelain v1 represents renames as "old -> new". Check both sides:
		// either side may be an included file whose content was attributed.
		if arrow := strings.LastIndex(name, " -> "); arrow >= 0 {
			paths = []string{strings.TrimSpace(name[:arrow]), strings.TrimSpace(name[arrow+4:])}
		}
		for _, candidate := range paths {
			rel, ok := sourceRelativePath(candidate, reg.Subpath)
			if !ok || !scopedIncludedPath(reg, rel) {
				continue
			}
			entries = append(entries, status+" "+rel)
		}
	}
	sort.Strings(entries)
	return uniqueStrings(entries), nil
}

func scopedIncludedPath(reg SourceRegistration, rel string) bool {
	if reg.Mode == ModeStructured && filepath.ToSlash(filepath.Clean(reg.StructuredManifest)) == rel {
		return true
	}
	if reg.Mode == ModeHybrid && reg.Overlay.Kind == "in_repo" && filepath.ToSlash(filepath.Clean(reg.Overlay.Path)) == rel {
		return true
	}
	return matchesAny(reg.Ingest.Include, rel) && !matchesAny(reg.Ingest.Exclude, rel)
}

func sourceRelativePath(candidate, subpath string) (string, bool) {
	candidate = filepath.ToSlash(strings.TrimSpace(candidate))
	subpath = filepath.ToSlash(filepath.Clean(subpath))
	if subpath == "." || subpath == "" {
		if candidate == "." || candidate == "" {
			return "", false
		}
		return candidate, true
	}
	if candidate == subpath {
		return "", false
	}
	prefix := strings.TrimSuffix(subpath, "/") + "/"
	if !strings.HasPrefix(candidate, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(candidate, prefix)
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}
