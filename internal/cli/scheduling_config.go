package cli

import (
	"github.com/zhoushoujianwork/memgov/internal/core"
	"gopkg.in/yaml.v3"
	"strconv"
)

func proactiveSchedulingOnly(before, after any) bool {
	b, ok := before.(*ProactiveApplication)
	if !ok || b == nil {
		return false
	}
	a, ok := after.(*ProactiveApplication)
	if !ok || a == nil {
		return false
	}
	x, y := *b, *a
	x.Scheduling, y.Scheduling = core.Scheduling{}, core.Scheduling{}
	x.Concurrency, y.Concurrency = 0, 0
	x.Batch, y.Batch = ApplicationBatch{}, ApplicationBatch{}
	return core.Digest(x) == core.Digest(y)
}

// Omission has defaults; an explicitly supplied zero never disables a deadline.
func validateSchedulingNodes(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i].Value, n.Content[i+1]
			switch key {
			case "analysis_concurrency", "execution_concurrency", "analysis_timeout_seconds", "execution_timeout_seconds", "review_timeout_seconds":
				v, e := strconv.Atoi(value.Value)
				if e != nil || v <= 0 {
					return core.Fail("invalid_input", "%s must be a finite positive integer", key)
				}
			}
		}
	}
	for _, child := range n.Content {
		if err := validateSchedulingNodes(child); err != nil {
			return err
		}
	}
	return nil
}
