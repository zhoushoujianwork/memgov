package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func dualOwnerPrivateFixture(t *testing.T) (string, core.RuntimeConfig) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if code, result := invoke(t, home, "", "init"); code != 0 {
		t.Fatal(result)
	}
	if code, result := invoke(t, home, "", "agent", "preset", "enable", "claude", "--name", "claude-default"); code != 0 {
		t.Fatal(result)
	}
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.owner.private.setup"}, func(tx *core.Tx) (any, error) {
		dws, err := tx.AddChannel(ctx, core.ChannelInput{Name: "dws-main", Kind: core.ChannelDwsPersonal, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "owner1", Profile: "corp1:owner1"}})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetChannelCapabilities(ctx, dws.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); err != nil {
			return nil, err
		}
		dws, err = core.ReadChannel(ctx, tx.Conn, dws.ID)
		if err != nil {
			return nil, err
		}
		if _, err = tx.AttestDWSOwner(ctx, dws.ID, dws.ConfigVersion); err != nil {
			return nil, err
		}
		app, err := tx.AddChannel(ctx, core.ChannelInput{Name: "app-main", Kind: core.ChannelDingTalkApp, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ClientID: "client1", RobotCode: "bot1", HistoryChannel: dws.ID}})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetChannelCapabilities(ctx, app.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "send": true}}, "fake"); err != nil {
			return nil, err
		}
		inbox, err := tx.AddRoute(ctx, app.ID, core.RouteInput{ConversationID: "cid:owner", ConversationType: "direct", Mode: "notify", SendPolicy: "draft_only"})
		if err != nil {
			return nil, err
		}
		delivery, err := tx.AddRoute(ctx, app.ID, core.RouteInput{ConversationID: "owner1", ConversationType: "direct", Mode: "notify", SendPolicy: "dispatch_only"})
		if err != nil {
			return nil, err
		}
		bash := false
		runtime, err := tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Name: "owner-private", Channel: app.ID, RouteIDs: []string{inbox.ID}, DeliveryRouteID: delivery.ID, Owner: core.Sender{IDType: "user_id", IDValue: "owner1"}, ApplicationMode: "direct", AgentBash: &bash, ExternalActions: "owner_confirmation", ClaudeProfile: "cc", ExecutionModel: "profile"})
		if err != nil {
			return nil, err
		}
		return tx.SetRuntimeStatus(ctx, runtime.ID, "stopped", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := core.ReadRuntime(ctx, store.DB, "owner-private")
	if err != nil {
		t.Fatal(err)
	}
	return home, runtime
}

func TestManagedRuntimeLegacyPermissionSnapshotAndRealDrift(t *testing.T) {
	home, original := dualOwnerPrivateFixture(t)
	cfg := configFile(t, "agents:\n  owner-chat: {preset: claude-default, memory_scope: owner_authorized, bash: false}\napplications:\n  owner_private: {enabled: true, runtime: owner-private, agent: owner-chat}\n")
	code, raw := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 || data(t, raw)["ready"] != true {
		t.Fatalf("restricted owner binding should be ready: %v", raw)
	}
	plan := data(t, raw)
	if code, raw = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "0"); code != 0 {
		t.Fatal(raw)
	}
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	current, err := core.ReadRuntime(ctx, store.DB, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	// This is the schema-18 runtime snapshot shape, before the Bash columns.
	legacy := core.Digest(map[string]any{"channel_id": current.ChannelID, "delivery_route_id": current.DeliveryRouteID, "owner_principal_id": current.OwnerPrincipalID, "owner_id_type": current.OwnerIDType, "owner_id_value": current.OwnerIDValue, "application_mode": current.ApplicationMode, "context_channel_id": current.ContextChannelID, "agent_capabilities": current.AgentCapabilities, "memory_scope": current.MemoryScope, "claude_profile": current.ClaudeProfile, "analysis_model": current.AnalysisModel, "execution_model": current.ExecutionModel, "agent_preset": current.AgentPreset, "item_threshold": current.ItemThreshold, "max_wait_seconds": current.MaxWaitSeconds, "reconcile_seconds": current.ReconcileSeconds})
	if core.Digest(runtimeManagedSnapshot(current)) != legacy {
		t.Fatal("new default-permission runtime snapshot differs from schema-18 shape")
	}
	applied, err := core.ReadAppliedConfig(ctx, store.DB, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range applied.Objects {
		if applied.Objects[i].Kind == "application" && applied.Objects[i].Name == "owner_private" {
			applied.Objects[i].Snapshot = legacy
		}
	}
	objects, err := json.Marshal(applied.Objects)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.legacy.manifest"}, func(tx *core.Tx) (any, error) {
		_, e := tx.Conn.ExecContext(ctx, "UPDATE applied_configs SET objects=? WHERE id=?", string(objects), applied.ID)
		return nil, e
	})
	if err != nil {
		t.Fatal(err)
	}
	code, raw = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 || data(t, raw)["ready"] != true || planContainsCode(t, raw, "managed_object_drift") {
		t.Fatalf("schema-18 default policy manifest was falsely reported as drift: %v", raw)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.actual.bash.drift"}, func(tx *core.Tx) (any, error) {
		_, e := tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET agent_bash=1 WHERE id=?", original.ID)
		return nil, e
	})
	if err != nil {
		t.Fatal(err)
	}
	code, raw = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 || !planContainsCode(t, raw, "managed_object_drift") {
		t.Fatalf("actual Bash grant was not reported as managed drift: %v", raw)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.actual.action.drift"}, func(tx *core.Tx) (any, error) {
		_, e := tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET agent_bash=0,external_actions='owner_request' WHERE id=?", original.ID)
		return nil, e
	})
	if err != nil {
		t.Fatal(err)
	}
	code, raw = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 || !planContainsCode(t, raw, "managed_object_drift") {
		t.Fatalf("actual action authority change was not reported as managed drift: %v", raw)
	}
}

func planContainsCode(t *testing.T, result map[string]any, code string) bool {
	t.Helper()
	for _, v := range data(t, result)["blockers"].([]any) {
		if v.(map[string]any)["code"] == code {
			return true
		}
	}
	return false
}

func TestOwnerPrivateBuiltInAndExplicitAgentPolicy(t *testing.T) {
	home, original := dualOwnerPrivateFixture(t)
	builtInConfig := configFile(t, "applications:\n  owner_private: {enabled: true, runtime: owner-private}\n")
	code, raw := invoke(t, home, "", "--config", builtInConfig, "config", "plan")
	if code != 0 {
		t.Fatal(raw)
	}
	plan := data(t, raw)
	if plan["ready"] != false || !planContainsCode(t, raw, "permission_expansion") {
		t.Fatalf("built-in full Bash must be shown as expansion: %v", plan)
	}
	if code, raw = invoke(t, home, "", "--config", builtInConfig, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "0", "--authorize-expansion", "--reason", "owner enabled private Bash"); code != 0 {
		t.Fatal(raw)
	}
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	current, err := core.ReadRuntime(ctx, store.DB, original.ID)
	store.Close()
	if err != nil || !current.AgentBash || current.ExternalActions != "owner_request" || current.OwnerPrincipalID != original.OwnerPrincipalID || current.DeliveryRouteID != original.DeliveryRouteID || current.RouteIDs[0] != original.RouteIDs[0] || current.ClaudeProfile != "cc" {
		t.Fatalf("built-in overlay changed identity, routing or model: %+v %v", current, err)
	}
	explicitConfig := configFile(t, "agents:\n  owner-chat:\n    preset: claude-default\n    memory_scope: owner_authorized\n    capabilities: [memory_read, local_read]\n    bash: false\n    external_actions: owner_confirmation\napplications:\n  owner_private: {enabled: true, runtime: owner-private, agent: owner-chat}\n")
	code, raw = invoke(t, home, "", "--config", explicitConfig, "config", "plan")
	if code != 0 {
		t.Fatal(raw)
	}
	plan = data(t, raw)
	if !planContainsCode(t, raw, "authorization_boundary_change") {
		t.Fatalf("Agent binding change must be fenced: %v", plan)
	}
	if code, raw = invoke(t, home, "", "--config", explicitConfig, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "1", "--authorize-boundary", "--reason", "owner selected restricted private Agent"); code != 0 {
		t.Fatal(raw)
	}
	store, err = core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	current, err = core.ReadRuntime(ctx, store.DB, original.ID)
	store.Close()
	if err != nil || current.AgentBash || current.ExternalActions != "owner_confirmation" || current.OwnerPrincipalID != original.OwnerPrincipalID || current.RouteIDs[0] != original.RouteIDs[0] || len(current.AgentCapabilities) != 2 {
		t.Fatalf("explicit false was not applied to same direct instance: %+v %v", current, err)
	}
}

func TestOwnerPrivatePlanRejectsMissingAndRunningDirectRuntime(t *testing.T) {
	home, original := dualOwnerPrivateFixture(t)
	missing := configFile(t, "applications:\n  owner_private: {enabled: true, runtime: missing-direct}\n")
	code, raw := invoke(t, home, "", "--config", missing, "config", "plan")
	if code != 0 || !planContainsCode(t, raw, "owner_private_runtime_missing") {
		t.Fatalf("missing direct runtime was not diagnosed: %v", raw)
	}
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.owner.private.running"}, func(tx *core.Tx) (any, error) {
		return tx.SetRuntimeStatus(ctx, original.ID, "running", "")
	})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	owner := configFile(t, "applications:\n  owner_private: {enabled: true, runtime: owner-private}\n")
	code, raw = invoke(t, home, "", "--config", owner, "config", "plan")
	if code != 0 || !planContainsCode(t, raw, "running_object_change") {
		t.Fatalf("running direct adoption was not blocked: %v", raw)
	}
}

func TestOwnerPrivatePlanRejectsRevokedIdentity(t *testing.T) {
	home, original := dualOwnerPrivateFixture(t)
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.owner.private.revoke"}, func(tx *core.Tx) (any, error) {
		_, e := tx.Conn.ExecContext(ctx, "UPDATE identity_aliases SET verified=0 WHERE principal_id=?", original.OwnerPrincipalID)
		return nil, e
	})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	owner := configFile(t, "applications:\n  owner_private: {enabled: true, runtime: owner-private}\n")
	code, raw := invoke(t, home, "", "--config", owner, "config", "plan")
	if code != 0 || !planContainsCode(t, raw, "owner_private_route_unverified") {
		t.Fatalf("revoked owner alias was not blocked: %v", raw)
	}
}
