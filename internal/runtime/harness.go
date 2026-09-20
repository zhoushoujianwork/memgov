package runtime

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// HarnessBundle is the execution contract used by Personal Jarvis. A harness
// supplies model analysis, task execution, confirmed actions, and memory
// review; channels, tasks, evidence, and permissions remain harness-neutral.
type HarnessBundle struct {
	Name     string
	Analyzer Analyzer
	Executor Executor
	Actioner ActionExecutor
	Reviewer Reviewer
}

// HarnessFactory creates one isolated harness for a runtime. Factories must
// not own channel routing or memory state; those belong to the host runtime.
type HarnessFactory func(analysisModel, executionModel string) (HarnessBundle, error)

// HarnessProfileSetter opts one harness component into the runtime's profile.
// SetProfile must be idempotent: a bundle may share one component across roles.
type HarnessProfileSetter interface {
	SetProfile(string)
}

// ConfigureProfile applies the same authentication profile to every role.
// Validate all roles first so a split harness cannot silently analyze or review
// using the ambient account while only its executor receives the profile.
func (b HarnessBundle) ConfigureProfile(profile string) error {
	components := []any{b.Analyzer, b.Executor, b.Reviewer}
	if b.Actioner != nil {
		components = append(components, b.Actioner)
	}
	for _, component := range components {
		if _, ok := component.(HarnessProfileSetter); !ok && profile != "" {
			return core.Fail("invalid_input", "harness %q does not support profile configuration for every role", b.Name)
		}
	}
	for _, component := range components {
		if setter, ok := component.(HarnessProfileSetter); ok {
			setter.SetProfile(profile)
		}
	}
	return nil
}

// HarnessDiagnostic is a side-effect-free summary of one registered harness.
// It intentionally reports only contract capabilities: model names and
// implementation details are owned by the harness and must not leak into
// runtime status output.
type HarnessDiagnostic struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Analyzer  bool   `json:"analyzer"`
	Executor  bool   `json:"executor"`
	Actioner  bool   `json:"actioner"`
	// ActionerFallback is true when Service can use the Executor's
	// ActionExecutor implementation because the bundle omitted Actioner.
	ActionerFallback bool   `json:"actioner_fallback,omitempty"`
	Reviewer         bool   `json:"reviewer"`
	Error            string `json:"error,omitempty"`
}

var harnessRegistry = struct {
	sync.RWMutex
	items map[string]HarnessFactory
}{items: map[string]HarnessFactory{}}

// RegisterHarness adds or replaces a named harness adapter. Names are stable
// configuration identifiers, not provider-specific implementation details.
func RegisterHarness(name string, factory HarnessFactory) error {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" || factory == nil {
		return fmt.Errorf("harness name and factory are required")
	}
	harnessRegistry.Lock()
	defer harnessRegistry.Unlock()
	harnessRegistry.items[name] = factory
	return nil
}

// HarnessNames returns the registered harness identifiers in deterministic
// order. Registration is intentionally process-local so callers can supply an
// adapter at startup without changing the persisted runtime schema.
func HarnessNames() []string {
	harnessRegistry.RLock()
	names := make([]string, 0, len(harnessRegistry.items))
	for name := range harnessRegistry.items {
		names = append(names, name)
	}
	harnessRegistry.RUnlock()
	sort.Strings(names)
	return names
}

// DiagnoseHarness constructs a harness through the same factory path used by
// the runtime and reports its contract. It never returns an error so a caller
// can display unavailable or incomplete adapters alongside healthy ones.
// Factories should remain lightweight constructors; this function does not
// invoke model binaries or perform network calls.
func DiagnoseHarness(name, analysisModel, executionModel string) HarnessDiagnostic {
	requested := strings.TrimSpace(strings.ToLower(name))
	if requested == "" {
		requested = "claude"
	}
	diagnostic := HarnessDiagnostic{Name: requested}
	bundle, err := NewHarness(requested, analysisModel, executionModel)
	if err != nil {
		diagnostic.Error = err.Error()
		return diagnostic
	}
	diagnostic.Available = true
	diagnostic.Analyzer = bundle.Analyzer != nil
	diagnostic.Executor = bundle.Executor != nil
	diagnostic.Actioner = bundle.Actioner != nil
	if !diagnostic.Actioner {
		_, diagnostic.ActionerFallback = bundle.Executor.(ActionExecutor)
	}
	diagnostic.Reviewer = bundle.Reviewer != nil
	return diagnostic
}

// DiagnoseHarnesses reports every registered harness and is suitable for
// startup checks and machine-readable CLI diagnostics.
func DiagnoseHarnesses(analysisModel, executionModel string) []HarnessDiagnostic {
	names := HarnessNames()
	out := make([]HarnessDiagnostic, 0, len(names))
	for _, name := range names {
		out = append(out, DiagnoseHarness(name, analysisModel, executionModel))
	}
	return out
}

// NewHarness resolves a configured harness without coupling the runtime to a
// particular CLI, model vendor, or sandbox implementation.
func NewHarness(name, analysisModel, executionModel string) (HarnessBundle, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		name = "claude"
	}
	harnessRegistry.RLock()
	factory := harnessRegistry.items[name]
	harnessRegistry.RUnlock()
	if factory == nil {
		return HarnessBundle{}, fmt.Errorf("unsupported agent harness %q", name)
	}
	bundle, err := factory(analysisModel, executionModel)
	if err != nil {
		return HarnessBundle{}, err
	}
	if bundle.Name == "" {
		bundle.Name = name
	}
	if bundle.Analyzer == nil || bundle.Executor == nil || bundle.Reviewer == nil {
		return HarnessBundle{}, fmt.Errorf("harness %q returned an incomplete adapter", name)
	}
	if bundle.Actioner == nil {
		if _, ok := bundle.Executor.(ActionExecutor); !ok {
			return HarnessBundle{}, fmt.Errorf("harness %q does not provide confirmed-action execution", name)
		}
	}
	return bundle, nil
}

func init() {
	_ = RegisterHarness("claude", func(analysisModel, executionModel string) (HarnessBundle, error) {
		model := NewClaude(analysisModel, executionModel)
		return HarnessBundle{Name: "claude", Analyzer: model, Executor: model, Actioner: model, Reviewer: model}, nil
	})
}
