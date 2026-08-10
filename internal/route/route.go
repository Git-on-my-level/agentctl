// Package route provides deterministic, advisory routing decisions.
// It never launches an agent or creates Multica work.
package route

import (
	"fmt"
	"strings"
)

// Lifecycle names the authority that should own a task lifecycle.
type Lifecycle string

const (
	LifecycleDirect  Lifecycle = "direct"
	LifecycleMultica Lifecycle = "multica"
)

// Request contains only explicit, observable routing inputs. Free-form prompt
// interpretation does not belong in this package.
type Request struct {
	ExplicitLifecycle Lifecycle
	ModelFamily       string
	NeedsPR           bool
	MultipleOwners    bool
	CrossHostHandoff  bool
	ParentMayExit     bool
	ReviewVisibility  bool
	MultiStage        bool
	LongLivedFixLoop  bool
}

// Decision is an explanation, not an authorization or dispatch record.
type Decision struct {
	Lifecycle Lifecycle `json:"lifecycle"`
	Adapter   string    `json:"adapter"`
	Reasons   []string  `json:"reasons"`
	Explicit  bool      `json:"explicit"`
}

// Explain returns a deterministic recommendation. Uncertainty intentionally
// resolves to direct execution because explicit promotion remains available.
func Explain(req Request) (Decision, error) {
	adapter, err := AdapterForModelFamily(req.ModelFamily)
	if err != nil {
		return Decision{}, err
	}

	if req.ExplicitLifecycle != "" {
		if req.ExplicitLifecycle != LifecycleDirect && req.ExplicitLifecycle != LifecycleMultica {
			return Decision{}, fmt.Errorf("unsupported lifecycle %q", req.ExplicitLifecycle)
		}
		return Decision{
			Lifecycle: req.ExplicitLifecycle,
			Adapter:   adapter,
			Reasons:   []string{"explicit_lifecycle"},
			Explicit:  true,
		}, nil
	}

	reasons := make([]string, 0, 7)
	if req.NeedsPR {
		reasons = append(reasons, "pr_lifecycle")
	}
	if req.MultipleOwners {
		reasons = append(reasons, "multiple_owners")
	}
	if req.CrossHostHandoff {
		reasons = append(reasons, "cross_host_handoff")
	}
	if req.ParentMayExit {
		reasons = append(reasons, "parent_may_exit")
	}
	if req.ReviewVisibility {
		reasons = append(reasons, "review_visibility")
	}
	if req.MultiStage {
		reasons = append(reasons, "multi_stage")
	}
	if req.LongLivedFixLoop {
		reasons = append(reasons, "long_lived_fix_loop")
	}

	if len(reasons) > 0 {
		return Decision{Lifecycle: LifecycleMultica, Adapter: adapter, Reasons: reasons}, nil
	}
	return Decision{
		Lifecycle: LifecycleDirect,
		Adapter:   adapter,
		Reasons:   []string{"direct_by_default"},
	}, nil
}

// AdapterForModelFamily keeps model execution ownership independent from
// lifecycle ownership. It recognizes reviewed family aliases only.
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
