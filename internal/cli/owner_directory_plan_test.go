package cli

import (
	"github.com/zhoushoujianwork/memgov/internal/core"
	"testing"
)

func TestOwnerDirectoryPlanRequiresReadAndRejectsUnsandboxedTests(t *testing.T) {
	enabled := true
	for _, tt := range []struct {
		name         string
		capabilities []string
		expected     string
	}{{"copy", []string{"local_read", "local_write"}, ""}, {"missing_read", []string{"local_write"}, "owner_directory_read_required"}, {"shell", []string{"local_read", "local_test"}, "owner_directory_test_sandbox_unavailable"}} {
		t.Run(tt.name, func(t *testing.T) {
			plan := DualConfigPlan{DefaultWorkspace: "global", Declaration: DualModeDeclaration{DataSources: map[string]DataSourceConfig{}, Agents: map[string]AgentDeclaration{"owner": {Directories: []string{"/authorized"}, Capabilities: tt.capabilities}}, Applications: ApplicationDeclarations{Proactive: &ProactiveApplication{Enabled: &enabled, Agent: "owner"}}}}
			blockers := []string{}
			validateDualPlanReferences(&plan, dualPlanState{Workspaces: map[string]core.Workspace{"global": {ID: "global"}}}, func(block bool, code, kind, name, summary string) {
				if block {
					blockers = append(blockers, code)
				}
			})
			if tt.expected == "" && len(blockers) != 0 {
				t.Fatalf("copy mode blocked: %v", blockers)
			}
			if tt.expected != "" && (len(blockers) != 1 || blockers[0] != tt.expected) {
				t.Fatalf("want %s got %v", tt.expected, blockers)
			}
		})
	}
}
