package runtime

import (
	"fmt"
	"strings"
	"sync"
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
	return bundle, nil
}

func init() {
	_ = RegisterHarness("claude", func(analysisModel, executionModel string) (HarnessBundle, error) {
		model := NewClaude(analysisModel, executionModel)
		return HarnessBundle{Name: "claude", Analyzer: model, Executor: model, Actioner: model, Reviewer: model}, nil
	})
}
