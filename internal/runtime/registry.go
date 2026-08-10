package runtime

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

// AdapterSpec is the complete configuration needed to reconstruct an adapter
// after restart. It is converted to source bindings on every execution.
type AdapterSpec struct {
	Name       string                 `json:"name"`
	Executable string                 `json:"executable,omitempty"`
	Multica    *adapter.MulticaConfig `json:"multica,omitempty"`
}

func (s AdapterSpec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return &Error{Code: CodeInvalidConfiguration, Message: "adapter name is required"}
	}
	if canonicalAdapterName(s.Name) != "multica" {
		if s.Multica != nil {
			return &Error{Code: CodeInvalidConfiguration, Message: "Multica configuration is only valid for the multica adapter"}
		}
		return nil
	}
	if s.Multica == nil {
		return &Error{Code: CodeInvalidConfiguration, Message: "Multica requires explicit profile, endpoint, and workspace configuration"}
	}
	for _, field := range []struct{ name, value string }{
		{"profile", s.Multica.Profile}, {"endpoint", s.Multica.Endpoint},
		{"workspace", s.Multica.Workspace},
	} {
		if strings.TrimSpace(field.value) == "" {
			return &Error{Code: CodeInvalidConfiguration, Message: "Multica " + field.name + " is required"}
		}
	}
	if parsed, err := url.Parse(s.Multica.Endpoint); err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return &Error{Code: CodeInvalidConfiguration, Message: "Multica endpoint must be an absolute credential-free URL"}
	}
	return nil
}

// Factory receives the exact stored specification. Implementations must not
// consult an ambient profile when constructing a Multica adapter.
type Factory func(AdapterSpec) (adapter.Adapter, error)

// Registry resolves reviewed adapter implementations. Direct adapters are
// retained so their in-process session tables remain observable; Multica is
// reconstructed from exact stored authority selectors on each resolution.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

func NewRegistry() *Registry { return &Registry{factories: map[string]Factory{}} }

func DefaultRegistry() *Registry {
	r := NewRegistry()
	registerSingleton := func(names []string, value adapter.Adapter) {
		factory := func(AdapterSpec) (adapter.Adapter, error) { return value, nil }
		for _, name := range names {
			_ = r.Register(name, factory)
		}
	}
	registerSingleton([]string{"codex"}, adapter.NewCodex())
	registerSingleton([]string{"cursor"}, adapter.NewCursor())
	registerSingleton([]string{"claude", "claude-code"}, adapter.NewClaudeCode())
	registerSingleton([]string{"omp"}, adapter.NewOMP())
	registerSingleton([]string{"generic", "generic-process"}, adapter.NewGenericProcess())
	_ = r.Register("multica", func(spec AdapterSpec) (adapter.Adapter, error) {
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		cfg := *spec.Multica
		if spec.Executable != "" {
			cfg.Binary = spec.Executable
		}
		return adapter.NewMultica(cfg), nil
	})
	return r
}

func (r *Registry) Register(name string, factory Factory) error {
	name = canonicalAdapterName(name)
	if name == "" || factory == nil {
		return errors.New("adapter name and factory are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("adapter %q is already registered", name)
	}
	r.factories[name] = factory
	return nil
}

func (r *Registry) Resolve(spec AdapterSpec) (adapter.Adapter, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	name := canonicalAdapterName(spec.Name)
	r.mu.RLock()
	factory := r.factories[name]
	r.mu.RUnlock()
	if factory == nil {
		return nil, &Error{Code: CodeNotFound, Adapter: name, Message: "adapter is not registered"}
	}
	value, err := factory(spec)
	if err != nil {
		return nil, wrapError("resolve_adapter", name, err)
	}
	if value == nil {
		return nil, &Error{Code: CodeInternal, Adapter: name, Message: "adapter factory returned nil"}
	}
	return value, nil
}

func canonicalAdapterName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude-code":
		return "claude"
	case "generic-process":
		return "generic"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}
