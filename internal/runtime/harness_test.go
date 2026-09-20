package runtime

import (
	"context"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type harnessTestModel struct{}

func (harnessTestModel) Analyze(context.Context, core.RuntimeBatch) (core.RuntimeAnalysis, ModelUsage, error) {
	return core.RuntimeAnalysis{}, ModelUsage{}, nil
}
func (harnessTestModel) Execute(context.Context, ExecutionInput) (core.RuntimeAttemptResult, error) {
	return core.RuntimeAttemptResult{}, nil
}
func (harnessTestModel) Review(context.Context, core.RuntimeTask, core.CandidateInput) (ReviewResult, error) {
	return ReviewResult{Decision: "accept"}, nil
}
func (harnessTestModel) ExecuteConfirmedAction(context.Context, ActionExecutionInput) (core.RuntimeAttemptResult, error) {
	return core.RuntimeAttemptResult{}, nil
}

func TestHarnessRegistryAllowsAlternativeAdapter(t *testing.T) {
	name := "test-harness"
	model := harnessTestModel{}
	if err := RegisterHarness(name, func(string, string) (HarnessBundle, error) {
		return HarnessBundle{Analyzer: model, Executor: model, Actioner: model, Reviewer: model}, nil
	}); err != nil {
		t.Fatal(err)
	}
	bundle, err := NewHarness(name, "analysis", "execution")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Name != name || bundle.Analyzer == nil || bundle.Executor == nil || bundle.Reviewer == nil {
		t.Fatalf("incomplete harness bundle: %+v", bundle)
	}
}

func TestHarnessRegistryRejectsUnknownAdapter(t *testing.T) {
	if _, err := NewHarness("missing-harness", "", ""); err == nil {
		t.Fatal("expected unknown harness error")
	}
}
