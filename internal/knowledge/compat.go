package knowledge

import (
	"fmt"
	"strconv"
	"strings"
)

// CurrentReaderVersion is the bundle reader contract implemented by this
// package. A bundle may require this version or an older compatible version.
const CurrentReaderVersion = "1.0.0"

var supportedBundleFeatures = map[string]bool{
	"knowledge_records": true,
	"lexical_index":     true,
}

func validateManifestCompatibility(manifest Manifest) error {
	if manifest.MinimumReader != "" {
		minimum, err := parseVersion(manifest.MinimumReader)
		if err != nil {
			return fmt.Errorf("invalid minimum_reader %q", manifest.MinimumReader)
		}
		current, _ := parseVersion(CurrentReaderVersion)
		if compareVersions(minimum, current) > 0 {
			return fmt.Errorf("bundle requires reader %s (current %s)", manifest.MinimumReader, CurrentReaderVersion)
		}
	}
	for _, feature := range manifest.Features {
		name := feature
		required := true
		if strings.HasPrefix(name, "optional:") {
			required = false
			name = strings.TrimPrefix(name, "optional:")
		} else if strings.HasPrefix(name, "required:") {
			name = strings.TrimPrefix(name, "required:")
		}
		if required && !supportedBundleFeatures[name] {
			return fmt.Errorf("bundle requires unsupported feature %q", name)
		}
	}
	return nil
}

func parseVersion(value string) ([]int, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "v"))
	if value == "" {
		return nil, fmt.Errorf("empty version")
	}
	parts := strings.Split(value, ".")
	out := make([]int, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("empty version component")
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid version component")
		}
		out[i] = n
	}
	return out, nil
}

func compareVersions(a, b []int) int {
	length := len(a)
	if len(b) > length {
		length = len(b)
	}
	for i := 0; i < length; i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}
