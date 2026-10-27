package agentidentity

import (
	"errors"
	"fmt"
	"sort"

	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

// Registry holds the adapters the composition root built from operator
// configuration. It is immutable after NewRegistry. An empty registry is
// valid: every governed session for a binding then denies with
// ReasonProviderNotConfigured.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry builds a registry. A nil provider, an invalid name or a
// duplicate name is refused.
func NewRegistry(providers ...Provider) (*Registry, error) {
	out := &Registry{providers: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		if p == nil {
			return nil, errors.New("agent identity registry: nil provider")
		}
		name := p.Name()
		if !identity.ValidProviderName(name) {
			return nil, fmt.Errorf("agent identity registry: invalid provider name %q", name)
		}
		if _, dup := out.providers[name]; dup {
			return nil, fmt.Errorf("agent identity registry: provider %q registered twice", name)
		}
		out.providers[name] = p
	}
	return out, nil
}

// Provider returns the adapter for name.
func (r *Registry) Provider(name string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.providers[name]
	return p, ok
}

// Names lists the registered provider names, sorted.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
