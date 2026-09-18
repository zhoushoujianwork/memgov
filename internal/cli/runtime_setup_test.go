package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel/dws"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

type setupStubAdapter struct {
	*stubAdapter
	discovered dws.RuntimeSetupResult
}

func (s *setupStubAdapter) DiscoverRuntimeSetup(context.Context, dws.RuntimeSetupRequest) (dws.RuntimeSetupResult, error) {
	return s.discovered, nil
}

func TestRuntimeSetupCreatesReadyConfigurationInOneCommand(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	adapter := &setupStubAdapter{
		stubAdapter: &stubAdapter{caps: core.Capabilities{Verified: map[string]bool{"history": true, "receive": true}}},
		discovered: dws.RuntimeSetupResult{Profile: "corp-a", CorpID: "corp-a", OwnerUserID: "owner-1", OwnerName: "Owner",
			RobotCode: "robot-1", RobotName: "Delivery", DeliveryID: "cid-direct", Conversations: []dws.RuntimeSetupConversation{
				{ID: "cid-group", Name: "Runtime Test"}, {ID: "cid-noise", Name: "Noise", Ignored: true},
			}},
	}
	code, value := invokeWith(t, adapter, home, "", "runtime", "setup", "watcher", "--workspace-path", project, "--pilot", "--claude-profile", "cc")
	if code != 0 {
		t.Fatalf("setup failed: %+v", value)
	}
	result := data(t, value)
	discovery := result["discovered"].(map[string]any)
	if discovery["group_discovery_complete"] != false || discovery["watched_group_count"] != float64(1) || discovery["ignored_count"] != float64(1) {
		t.Fatalf("partial scope was not disclosed: %+v", discovery)
	}
	runtimeConfig := result["runtime"].(map[string]any)
	if runtimeConfig["name"] != "watcher" || runtimeConfig["item_threshold"].(float64) != 1 || runtimeConfig["max_wait_seconds"].(float64) != 30 {
		t.Fatalf("unexpected runtime config: %+v", runtimeConfig)
	}
	if runtimeConfig["claude_profile"] != "cc" || runtimeConfig["analysis_model"] != "haiku" || runtimeConfig["execution_model"] != "profile" {
		t.Fatalf("Claude profile defaults were not stored: %+v", runtimeConfig)
	}
	channel := result["channel"].(map[string]any)
	identity := channel["identity"].(map[string]any)
	if identity["delivery_robot_name"] != "Delivery" {
		t.Fatalf("delivery robot name was not stored for group filtering: %+v", identity)
	}
	routes := channel["routes"].([]any)
	if len(routes) != 2 {
		t.Fatalf("expected watched and ignored routes only: %+v", routes)
	}
	ignored := routes[1].(map[string]any)
	if ignored["conversation_id"] != "cid-noise" || ignored["mode"] != "ignore" {
		t.Fatalf("ignored route was not marked: %+v", ignored)
	}
	if runtimeConfig["completion_policy"] != "record_only" || runtimeConfig["external_actions"] != "owner_delegated" || runtimeConfig["agent_bash"] != true {
		t.Fatalf("Cyber owner policy was not configured: %+v", runtimeConfig)
	}
	if runtimeConfig["owner_id_type"] != "user_id" || runtimeConfig["owner_id_value"] != "owner-1" {
		t.Fatalf("owner identity namespace is incorrect: %+v", runtimeConfig)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "claude-default", "agent.yaml")); err != nil {
		t.Fatalf("preset was not created: %v", err)
	}
}

func TestRuntimeSetupCannotActivateOnlyIgnoredPartialGroups(t *testing.T) {
	adapter := &setupStubAdapter{
		stubAdapter: &stubAdapter{},
		discovered:  dws.RuntimeSetupResult{Profile: "corp", CorpID: "corp", OwnerUserID: "owner", RobotCode: "bot", RobotName: "Bot", DeliveryID: "owner", Conversations: []dws.RuntimeSetupConversation{{ID: "cid-ignore", Ignored: true}}},
	}
	code, value := invokeWith(t, adapter, t.TempDir(), "", "runtime", "setup", "watcher", "--workspace-path", t.TempDir())
	if code == 0 {
		t.Fatalf("only ignored groups activated: %+v", value)
	}
}
