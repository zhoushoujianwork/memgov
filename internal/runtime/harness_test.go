package runtime

import (
	"context"
	"sort"
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

func TestHarnessDiagnosticsAreDeterministicAndExposeContract(t *testing.T) {
	name := "diagnostic-harness"
	model := harnessTestModel{}
	if err := RegisterHarness(name, func(string, string) (HarnessBundle, error) {
		return HarnessBundle{Analyzer: model, Executor: model, Actioner: model, Reviewer: model}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := DiagnoseHarness(name, "", ""); !got.Available || !got.Analyzer || !got.Executor || !got.Actioner || !got.Reviewer || got.Error != "" {
		t.Fatalf("unexpected diagnostic: %+v", got)
	}
	names := HarnessNames()
	if !sort.StringsAreSorted(names) {
		t.Fatalf("harness names are not sorted: %v", names)
	}
	if all := DiagnoseHarnesses("", ""); len(all) != len(names) {
		t.Fatalf("diagnostic count %d != names %d", len(all), len(names))
	}
	if got := DiagnoseHarness("missing-harness", "", ""); got.Available || got.Error == "" {
		t.Fatalf("unknown harness was reported healthy: %+v", got)
	}
}
