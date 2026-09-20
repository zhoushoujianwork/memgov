package channel

import (
	"strings"
	"sync"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// AdapterFactory creates an adapter for one channel kind. The factory must not
// receive channel configuration or secrets: ConfigFor is the only place where
// the runtime binds a stored channel to an adapter. Keeping factories keyed by
// kind lets a deployment replace a platform implementation without changing
// the message, task, or memory layers.
type AdapterFactory func() Adapter

// Registry maps provider-neutral channel kinds to adapter factories. It is
// intentionally independent of concrete platforms (DingTalk, Slack, etc.) so
// callers can register adapters at composition time and tests can use multiple
// fake platforms in one process.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]AdapterFactory
}

// NewRegistry returns an empty registry. Built-in adapters are registered by
// the composition root; the channel package never imports a platform package.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]AdapterFactory)}
}

func normalizeAdapterKind(kind string) string {
	return strings.TrimSpace(kind)
}

// Register adds a factory for kind. Duplicate registration is rejected so an
// accidental second plugin cannot silently replace a live platform adapter.
// Use Replace when an embedding application deliberately selects another
// implementation for the same kind.
func (r *Registry) Register(kind string, factory AdapterFactory) error {
	kind = normalizeAdapterKind(kind)
	if kind == "" || factory == nil {
		return core.Fail("invalid_input", "adapter kind and factory are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = make(map[string]AdapterFactory)
	}
	if _, exists := r.factories[kind]; exists {
		return core.Fail("conflict", "adapter kind %q is already registered", kind)
	}
	r.factories[kind] = factory
	return nil
}

// Replace intentionally swaps the factory for kind. Existing adapter
// instances are owned by the caller and are not stopped or mutated.
func (r *Registry) Replace(kind string, factory AdapterFactory) error {
	kind = normalizeAdapterKind(kind)
	if kind == "" || factory == nil {
		return core.Fail("invalid_input", "adapter kind and factory are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = make(map[string]AdapterFactory)
	}
	r.factories[kind] = factory
	return nil
}

// Lookup returns a factory without constructing an adapter. The returned
// function is safe to invoke after the registry is unlocked.
func (r *Registry) Lookup(kind string) (AdapterFactory, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	factory, ok := r.factories[normalizeAdapterKind(kind)]
	r.mu.RUnlock()
	return factory, ok
}
