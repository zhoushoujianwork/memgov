package runtime

import (
	"context"
	"sort"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type harnessTestModel struct{}

type profiledHarnessModel struct {
	harnessTestModel
	profile string
}

func (m *profiledHarnessModel) SetProfile(profile string) { m.profile = profile }

type executorWithoutActions struct{}

func (executorWithoutActions) Execute(context.Context, ExecutionInput) (core.RuntimeAttemptResult, error) {
	return core.RuntimeAttemptResult{}, nil
}

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

func TestHarnessConfiguresProfileAcrossSeparateComponents(t *testing.T) {
	analyzer, executor, actioner, reviewer := &profiledHarnessModel{}, &profiledHarnessModel{}, &profiledHarnessModel{}, &profiledHarnessModel{}
	bundle := HarnessBundle{Name: "split", Analyzer: analyzer, Executor: executor, Actioner: actioner, Reviewer: reviewer}
	if err := bundle.ConfigureProfile("selected-account"); err != nil {
		t.Fatal(err)
	}
	for role, component := range map[string]*profiledHarnessModel{"analyzer": analyzer, "executor": executor, "actioner": actioner, "reviewer": reviewer} {
		if component.profile != "selected-account" {
			t.Errorf("%s used profile %q", role, component.profile)
		}
	}
	if err := bundle.ConfigureProfile(""); err != nil {
		t.Fatal(err)
	}
	if analyzer.profile != "" || executor.profile != "" || actioner.profile != "" || reviewer.profile != "" {
		t.Fatal("clearing a profile left a component on the previous account")
	}
}

func TestHarnessRejectsPartiallySupportedProfileWithoutChangingComponents(t *testing.T) {
	executor := &profiledHarnessModel{profile: "previous"}
	bundle := HarnessBundle{Name: "mixed", Analyzer: harnessTestModel{}, Executor: executor, Reviewer: harnessTestModel{}}
	if err := bundle.ConfigureProfile("selected-account"); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("unsupported profile accepted: %v", err)
	}
	if executor.profile != "previous" {
		t.Fatal("failed profile configuration partially changed the harness")
	}
	if err := bundle.ConfigureProfile(""); err != nil {
		t.Fatalf("native harness authentication should remain available: %v", err)
	}
}

func TestHarnessRegistryRequiresConfirmedActionExecution(t *testing.T) {
	name := "no-action-harness"
	model := harnessTestModel{}
	if err := RegisterHarness(name, func(string, string) (HarnessBundle, error) {
		return HarnessBundle{Analyzer: model, Executor: executorWithoutActions{}, Reviewer: model}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHarness(name, "", ""); err == nil {
		t.Fatal("harness without confirmed-action execution was accepted")
	}
	if diagnostic := DiagnoseHarness(name, "", ""); diagnostic.Available {
		t.Fatalf("unstartable harness was reported available: %+v", diagnostic)
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
