// Package route provides deterministic, advisory routing decisions.
// It never launches an agent or creates remote work.
package route

import (
	"fmt"
	"strings"
)

// AdapterForModelFamily maps reviewed family aliases onto a native adapter
// name. Unknown families fail closed so callers pick an adapter explicitly.
func AdapterForModelFamily(family string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(family))
	switch normalized {
	case "", "auto":
		return "", nil
	case "gpt", "openai", "openai-gpt", "codex":
		return "codex", nil
	case "claude", "anthropic", "anthropic-claude":
		return "claude", nil
	case "cursor", "composer", "grok", "cursor-composer", "cursor-grok":
		return "cursor", nil
	case "glm", "open-weight", "open_weight", "openweight", "omp":
		return "omp", nil
	default:
		return "", fmt.Errorf("unknown model family %q; select an adapter explicitly", family)
	}
}
