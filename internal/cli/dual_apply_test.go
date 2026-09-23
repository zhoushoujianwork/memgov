package cli

import (
	"context"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDualApplyRequiresCurrentPlanAndCreatesImmutableVersion(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	cfg := configFile(t, "data_sources: {}\nagents: {}\napplications: {}\n")
	if code, result := invoke(t, home, "", "init"); code != 0 {
		t.Fatal(result)
	}
	code, result := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	plan := data(t, result)
	if plan["ready"] != true {
		t.Fatalf("empty config should be ready: %v", plan)
	}
	digest := plan["plan_digest"].(string)
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", digest, "--expected-version", "0"); code != 0 {
		t.Fatal(result)
	}
	applied := data(t, result)
	if applied["version"] != float64(1) {
		t.Fatalf("version not persisted: %v", applied)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", digest, "--expected-version", "0"); code == 0 {
		t.Fatalf("stale plan was accepted: %v", result)
	}
	code, result = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	newPlan := data(t, result)
	if newPlan["applied_version"] != float64(1) {
		t.Fatalf("plan missed applied version: %v", newPlan)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", newPlan["plan_digest"].(string), "--expected-version", "1"); code != 0 {
		t.Fatal(result)
	}
	if data(t, result)["version"] != float64(1) {
		t.Fatalf("idempotent reapply changed version: %v", result)
	}
}

func TestDualApplyRejectsUnresolvedSourceWithoutStateMutation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	cfg := configFile(t, "data_sources:\n  work_chat:\n    channel: dws-main\n    groups:\n      member_robot: app-main\n")
	if code, result := invoke(t, home, "", "init"); code != 0 {
		t.Fatal(result)
	}
	code, result := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	plan := data(t, result)
	if plan["ready"] != false {
		t.Fatalf("unresolved source ready: %v", plan)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "0"); code == 0 {
		t.Fatalf("unresolved source applied: %v", result)
	}
	code, result = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	if data(t, result)["applied_version"] != float64(0) {
		t.Fatalf("rejected apply mutated applied state: %v", result)
	}
}

func TestDualApplyPersistsSourceIgnoreAndRequiresExpansionReason(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if code, result := invoke(t, home, "", "init"); code != 0 {
		t.Fatal(result)
	}
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.dual.channels"}, func(tx *core.Tx) (any, error) {
		dws, err := tx.AddChannel(ctx, core.ChannelInput{Name: "dws-main", Kind: core.ChannelDwsPersonal, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "owner1", Profile: "corp1:owner1", DeliveryRobotCode: "bot1", DeliveryRobotName: "Bot"}})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetChannelCapabilities(ctx, dws.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true, "send": true}}, "fake"); err != nil {
			return nil, err
		}
		return tx.AddChannel(ctx, core.ChannelInput{Name: "app-main", Kind: core.ChannelDingTalkApp, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ClientID: "client1", RobotCode: "bot1"}})
	})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	cfg := configFile(t, "data_sources:\n  work_chat:\n    channel: dws-main\n    groups:\n      member_robot: app-main\n      ignore: [Noise]\n")
	code, result := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	plan := data(t, result)
	if plan["ready"] != true {
		t.Fatalf("source should be ready: %v", plan)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "0"); code != 0 {
		t.Fatal(result)
	}
	store, err = core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	source, err := core.ReadDataSource(ctx, store.DB, "work_chat")
	store.Close()
	if err != nil || len(source.Ignore) != 1 || source.Ignore[0] != "Noise" || source.MemberRobotCode != "bot1" {
		t.Fatalf("source policy not applied: %+v %v", source, err)
	}
	expanded := configFile(t, "data_sources:\n  work_chat:\n    channel: dws-main\n    groups:\n      member_robot: app-main\n      ignore: []\n")
	code, result = invoke(t, home, "", "--config", expanded, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	plan = data(t, result)
	if plan["ready"] != false {
		t.Fatalf("scope expansion should require authorization: %v", plan)
	}
	if code, result = invoke(t, home, "", "--config", expanded, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "1"); code == 0 {
		t.Fatalf("scope expansion silently applied: %v", result)
	}
	if code, result = invoke(t, home, "", "--config", expanded, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "1", "--authorize-expansion", "--reason", "owner widened source scope"); code != 0 {
		t.Fatal(result)
	}
	if data(t, result)["version"] != float64(2) {
		t.Fatalf("authorized scope change not versioned: %v", result)
	}
}

func TestDualApplyCreatesProactiveRuntimeBoundToSource(t *testing.T) {
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
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.proactive.channels"}, func(tx *core.Tx) (any, error) {
		dws, err := tx.AddChannel(ctx, core.ChannelInput{Name: "dws-main", Kind: core.ChannelDwsPersonal, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "owner1", Profile: "corp1:owner1", DeliveryRobotCode: "bot1", DeliveryRobotName: "Bot"}})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetChannelCapabilities(ctx, dws.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true, "send": true}}, "fake"); err != nil {
			return nil, err
		}
		if _, err = tx.AddChannel(ctx, core.ChannelInput{Name: "app-main", Kind: core.ChannelDingTalkApp, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ClientID: "client1", RobotCode: "bot1"}}); err != nil {
			return nil, err
		}
		if _, err = tx.AddRoute(ctx, dws.ID, core.RouteInput{ConversationID: "cid:work", ConversationType: "group", Mode: "collect"}); err != nil {
			return nil, err
		}
		if _, err = tx.AddRoute(ctx, dws.ID, core.RouteInput{ConversationID: "cid:legacy", ConversationType: "group", Mode: "collect"}); err != nil {
			return nil, err
		}
		if _, err = tx.AddRoute(ctx, dws.ID, core.RouteInput{ConversationID: "cid:never", ConversationType: "group", Mode: "collect"}); err != nil {
			return nil, err
		}
		direct, err := tx.AddRoute(ctx, dws.ID, core.RouteInput{ConversationID: "owner1", ConversationType: "direct", Mode: "notify"})
		if err != nil {
			return nil, err
		}
		if _, err = tx.UpdateRoute(ctx, direct.ID, direct.Version, core.RouteInput{SendPolicy: "dispatch_only"}, "enable owner delivery"); err != nil {
			return nil, err
		}
		return tx.Intake(ctx, dws.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "owner-bootstrap", ConversationID: "owner1", ConversationType: "direct", Tenant: dws.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "owner1"}, Body: "hello", Format: "text", SentAt: core.Now(), EventAt: core.Now()})
	})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	cfg := configFile(t, "data_sources:\n  work_chat:\n    channel: dws-main\n    groups:\n      member_robot: app-main\nagents:\n  owner-assistant:\n    preset: claude-default\n    capabilities: [conversation_history_read]\napplications:\n  proactive:\n    enabled: true\n    source: work_chat\n    owner: {id_type: user_id, id_value: owner1}\n    agent: owner-assistant\n")
	code, result := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	plan := data(t, result)
	if plan["ready"] != true {
		t.Fatalf("proactive config unexpectedly blocked: %v", plan)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "0"); code != 0 {
		t.Fatal(result)
	}
	store, err = core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = core.ReadRuntime(ctx, store.DB, "proactive"); core.ErrorCode(err) != "not_found" {
		t.Fatalf("first apply created a watcher before positive group discovery: %v", err)
	}
	source, err := core.ReadDataSource(ctx, store.DB, "work_chat")
	if err != nil || len(source.RouteIDs) != 0 {
		t.Fatalf("new source inherited legacy groups: source=%+v error=%v", source, err)
	}
	code, result = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	deferredPlan := data(t, result)
	if got := dualApplicationAction(deferredPlan, "proactive"); got != "deferred" {
		t.Fatalf("empty source scope was not shown as deferred: action=%s plan=%v", got, deferredPlan)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.source.positive.one"}, func(tx *core.Tx) (any, error) {
		d, e := core.ReadDataSource(ctx, tx.Conn, source.ID)
		if e != nil {
			return nil, e
		}
		channel, e := core.ReadChannel(ctx, tx.Conn, d.ChannelID)
		if e != nil {
			return nil, e
		}
		groups := []core.DataSourceGroup{{ID: "cid:work"}}
		if e = tx.RecordSourceGroupDiscovery(ctx, d.ID, d.Version, channel.ConfigVersion, groups); e != nil {
			return nil, e
		}
		// Simulate a partial discovery: collection retains three old routes,
		// but only this one group has current positive robot/activity proof.
		return tx.SyncDataSourceGroupDetails(ctx, d.ID, []core.DataSourceGroup{{ID: "cid:work"}, {ID: "cid:legacy"}, {ID: "cid:never"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	// Discovery changes only route_ids, not YAML or source config version. The
	// earlier plan digest must still be fenced off.
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", deferredPlan["plan_digest"].(string), "--expected-version", dualAppliedVersion(deferredPlan)); code == 0 {
		t.Fatalf("stale source scope plan applied: %v", result)
	}
	code, result = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	positivePlan := data(t, result)
	if got := dualApplicationAction(positivePlan, "proactive"); got != "create" {
		t.Fatalf("verified source scope did not resume deferred watcher: action=%s plan=%v", got, positivePlan)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", positivePlan["plan_digest"].(string), "--expected-version", dualAppliedVersion(positivePlan)); code != 0 {
		t.Fatal(result)
	}
	runtime, err := core.ReadRuntime(ctx, store.DB, "proactive")
	if err != nil {
		t.Fatal(err)
	}
	source, bound, err := core.DataSourceForRuntime(ctx, store.DB, runtime.ID)
	if err != nil || !bound || source.Name != "work_chat" || runtime.Status != "stopped" || len(runtime.AgentCapabilities) != 1 || len(source.RouteIDs) != 3 || len(runtime.RouteIDs) != 1 {
		t.Fatalf("runtime did not adopt exactly one verified source group: %+v %+v %v %v", runtime, source, bound, err)
	}
	positiveRoute, err := core.ReadRoute(ctx, store.DB, runtime.RouteIDs[0])
	if err != nil || positiveRoute.ConversationID != "cid:work" {
		t.Fatalf("runtime selected an old unproved group: route=%+v error=%v", positiveRoute, err)
	}
	code, result = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	beforeReceiptChange := data(t, result)
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.source.receipt.invalid"}, func(tx *core.Tx) (any, error) {
		return nil, tx.InvalidateSourceGroupDiscovery(ctx, source.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", beforeReceiptChange["plan_digest"].(string), "--expected-version", dualAppliedVersion(beforeReceiptChange)); code == 0 {
		t.Fatalf("receipt changed without route_ids but old plan still applied: %v", result)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.source.positive.two"}, func(tx *core.Tx) (any, error) {
		d, e := core.ReadDataSource(ctx, tx.Conn, source.ID)
		if e != nil {
			return nil, e
		}
		channel, e := core.ReadChannel(ctx, tx.Conn, d.ChannelID)
		if e != nil {
			return nil, e
		}
		groups := []core.DataSourceGroup{{ID: "cid:work"}, {ID: "cid:legacy"}}
		if e = tx.RecordSourceGroupDiscovery(ctx, d.ID, d.Version, channel.ConfigVersion, groups); e != nil {
			return nil, e
		}
		return tx.SyncDataSourceGroupDetails(ctx, d.ID, groups)
	})
	if err != nil {
		t.Fatal(err)
	}
	code, result = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	refreshPlan := data(t, result)
	if got := dualApplicationAction(refreshPlan, "proactive"); got != "refresh_scope" {
		t.Fatalf("same YAML did not refresh stopped source scope: action=%s plan=%v", got, refreshPlan)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", refreshPlan["plan_digest"].(string), "--expected-version", dualAppliedVersion(refreshPlan)); code != 0 {
		t.Fatal(result)
	}
	runtime, err = core.ReadRuntime(ctx, store.DB, "proactive")
	source, _, _ = core.DataSourceForRuntime(ctx, store.DB, runtime.ID)
	if err != nil || len(runtime.RouteIDs) != 2 || !sameRouteScope(runtime.RouteIDs, source.RouteIDs) {
		t.Fatalf("proactive scope widened past verified source routes: runtime=%+v source=%+v error=%v", runtime, source, err)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.source.none"}, func(tx *core.Tx) (any, error) {
		return tx.SyncDataSourceGroupDetails(ctx, source.ID, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	code, result = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	emptyPlan := data(t, result)
	if got := dualApplicationAction(emptyPlan, "proactive"); got != "defer_scope" {
		t.Fatalf("source scope withdrawal did not suspend stopped watcher: action=%s plan=%v", got, emptyPlan)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", emptyPlan["plan_digest"].(string), "--expected-version", dualAppliedVersion(emptyPlan)); code != 0 {
		t.Fatal(result)
	}
	runtime, err = core.ReadRuntime(ctx, store.DB, "proactive")
	if err != nil || len(runtime.RouteIDs) != 0 || runtime.Status != "stopped" {
		t.Fatalf("withdrawn source scope left a runnable watcher: runtime=%+v error=%v", runtime, err)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.proactive.start.empty"}, func(tx *core.Tx) (any, error) {
		return tx.SetRuntimeStatus(ctx, runtime.ID, "running", "")
	})
	if core.ErrorCode(err) != "denied" {
		t.Fatalf("watcher started without positive source groups: %v", err)
	}
}

func dualApplicationAction(plan map[string]any, name string) string {
	changes, _ := plan["changes"].([]any)
	for _, item := range changes {
		change, _ := item.(map[string]any)
		if change["kind"] == "application" && change["name"] == name {
			action, _ := change["action"].(string)
			return action
		}
	}
	return ""
}

func dualAppliedVersion(plan map[string]any) string {
	version, _ := plan["applied_version"].(float64)
	return strconv.Itoa(int(version))
}

func groupApplyFixture(t *testing.T) (string, core.Channel, core.Channel) {
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
	var dws, app core.Channel
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.group.channels"}, func(tx *core.Tx) (any, error) {
		var err error
		dws, err = tx.AddChannel(ctx, core.ChannelInput{Name: "dws-main", Kind: core.ChannelDwsPersonal, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "owner1", Profile: "corp1:owner1", DeliveryRobotCode: "bot1", DeliveryRobotName: "Bot"}})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetChannelCapabilities(ctx, dws.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true, "send": true}}, "fake"); err != nil {
			return nil, err
		}
		dws, err = core.ReadChannel(ctx, tx.Conn, dws.ID)
		if err != nil {
			return nil, err
		}
		if _, err = tx.AttestDWSOwner(ctx, dws.ID, dws.ConfigVersion); err != nil {
			return nil, err
		}
		app, err = tx.AddChannel(ctx, core.ChannelInput{Name: "app-main", Kind: core.ChannelDingTalkApp, Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ClientID: "client1", RobotCode: "bot1", HistoryChannel: dws.ID}})
		if err != nil {
			return nil, err
		}
		if _, err = tx.SetChannelCapabilities(ctx, app.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "send": true}}, "fake"); err != nil {
			return nil, err
		}
		if _, err = tx.AddRoute(ctx, dws.ID, core.RouteInput{ConversationID: "cid:work", ConversationType: "group", Mode: "collect"}); err != nil {
			return nil, err
		}
		appRoute, err := tx.AddRoute(ctx, app.ID, core.RouteInput{ConversationID: "cid:work", ConversationType: "group", Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation"})
		if err != nil {
			return nil, err
		}
		if _, err = tx.UpdateRoute(ctx, appRoute.ID, appRoute.Version, core.RouteInput{SendPolicy: "reply_to_trigger"}, "enable explicit group replies"); err != nil {
			return nil, err
		}
		if _, err = tx.AddRoute(ctx, dws.ID, core.RouteInput{ConversationID: "owner1", ConversationType: "direct", Mode: "notify"}); err != nil {
			return nil, err
		}
		direct, err := tx.AddRoute(ctx, app.ID, core.RouteInput{ConversationID: "owner1", ConversationType: "direct", Mode: "notify"})
		if err != nil {
			return nil, err
		}
		if _, err = tx.UpdateRoute(ctx, direct.ID, direct.Version, core.RouteInput{SendPolicy: "dispatch_only"}, "enable owner confirmation delivery"); err != nil {
			return nil, err
		}
		return tx.Intake(ctx, dws.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "owner-bootstrap", ConversationID: "owner1", ConversationType: "direct", Tenant: dws.Tenant, Sender: core.Sender{IDType: "user_id", IDValue: "owner1"}, Body: "hello", Format: "text", SentAt: core.Now(), EventAt: core.Now()})
	})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	return home, dws, app
}

func TestDualApplyCreatesGroupAgentWithBoundDWSContext(t *testing.T) {
	home, dws, app := groupApplyFixture(t)
	cfg := configFile(t, "data_sources:\n  work_chat:\n    channel: dws-main\n    groups:\n      member_robot: app-main\nagents:\n  group-helper:\n    preset: claude-default\n    capabilities: [conversation_history_read]\napplications:\n  group_mention:\n    enabled: true\n    source: work_chat\n    channel: app-main\n    default_agent: group-helper\n")
	code, result := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(result)
	}
	plan := data(t, result)
	if plan["ready"] != true {
		t.Fatalf("group config unexpectedly blocked: %v", plan)
	}
	if code, result = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "0"); code != 0 {
		t.Fatal(result)
	}
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime, err := core.ReadRuntime(ctx, store.DB, "group-mention")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ApplicationMode != "group_mention" || runtime.ContextChannelID != dws.ID || runtime.ChannelID != app.ID || len(runtime.RouteIDs) != 1 {
		t.Fatalf("group Agent configuration: %+v", runtime)
	}
	if runtime.Concurrency != 4 {
		t.Fatalf("group execution default must permit independent members: %d", runtime.Concurrency)
	}
}
