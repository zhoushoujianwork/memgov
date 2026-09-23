package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestProactiveRecordOnlyAndDelegatedDefaults(t *testing.T) {
	body := `data_sources:
  watched: {channel: dws-main}
applications:
  proactive:
    enabled: true
    source: watched
    owner: {id_type: user_id, id_value: owner1}
    delivery: owner_direct
`
	a := &app{}
	if err := a.loadConfig([]byte(body)); err != nil {
		t.Fatal(err)
	}
	v, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := v.Declaration.Applications.Proactive
	agent := v.Declaration.Agents[p.Agent]
	if p.Delivery != "record_only" || !agent.Bash || agent.ExternalActions != "owner_delegated" || agent.Skills.Inherit != "executor" || !hasConfigString(agent.Capabilities, "local_test") {
		t.Fatalf("background defaults: %+v %+v", p, agent)
	}
	if len(v.Diagnostics) != 1 || v.Diagnostics[0].Code != "proactive_delivery_migrated" {
		t.Fatalf("legacy migration not diagnosed: %+v", v.Diagnostics)
	}
	restricted := strings.Replace(body, "    delivery: owner_direct", "    agent: limited", 1) + "agents:\n  limited: {bash: false, capabilities: [], skills: {inherit: none}, external_actions: owner_confirmation}\n"
	a = &app{}
	if err := a.loadConfig([]byte(restricted)); err != nil {
		t.Fatal(err)
	}
	v, err = NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	agent = v.Declaration.Agents["limited"]
	if agent.Bash || len(agent.Capabilities) != 0 || agent.ExternalActions != "owner_confirmation" || agent.Skills.Inherit != "none" {
		t.Fatalf("explicit restriction expanded: %+v", agent)
	}
}

func TestBotPersonasNormalizeOwnerAndGroupIndependently(t *testing.T) {
	body := `agents:
  helper: {preset: claude-default, execution_model: bot-model}
  special: {preset: claude-default, capabilities: []}
applications:
  bots:
    app-main:
      default_agent: helper
      owner_private: {enabled: true, runtime: owner-private}
      group_mention:
        enabled: true
        bindings: [{conversation_id: group1, agent: special}]
`
	a := &app{}
	if err := a.loadConfig([]byte(body)); err != nil {
		t.Fatal(err)
	}
	v, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	bot := v.Declaration.Applications.Bots["app-main"]
	owner := v.Declaration.Agents[bot.OwnerPrivate.Agent]
	group := v.Declaration.Agents[bot.GroupMention.DefaultAgent]
	if !owner.Bash || owner.ExternalActions != "owner_request" || owner.ExecutionModel != "bot-model" {
		t.Fatalf("owner default did not inherit persona with full permissions: %+v", owner)
	}
	if group.Bash || group.ExternalActions != "owner_confirmation" {
		t.Fatalf("group acquired owner permissions: %+v", group)
	}
	for _, bad := range []string{
		strings.Replace(body, "default_agent: helper", "default_agent: absent", 1),
		strings.Replace(body, "  bots:", "  owner_private: {enabled: true, runtime: owner-private}\n  bots:", 1),
		strings.Replace(body, "  bots:", "  group_mention: {channel: app-main, enabled: false}\n  bots:", 1),
		strings.Replace(body, "capabilities: []", "capabilities: [], external_actions: owner_delegated", 1),
	} {
		candidate := &app{}
		if err := candidate.loadConfig([]byte(bad)); err == nil {
			t.Fatalf("invalid conflicting/privileged bot config accepted:\n%s", bad)
		}
	}
}

func TestDualBotsPlanApplyAndServiceWithoutObservationSource(t *testing.T) {
	home, private := dualOwnerPrivateFixture(t)
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.bot.channels"}, func(tx *core.Tx) (any, error) {
		first, e := core.ReadChannel(ctx, tx.Conn, "app-main")
		if e != nil {
			return nil, e
		}
		second, e := tx.AddChannel(ctx, core.ChannelInput{Name: "app-second", Kind: core.ChannelDingTalkApp, Identity: core.ChannelIdentity{ExpectedCorpID: first.Tenant, ClientID: "second", RobotCode: "second"}})
		if e != nil {
			return nil, e
		}
		if _, e = tx.SetChannelCapabilities(ctx, second.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "send": true}}, "fixture"); e != nil {
			return nil, e
		}
		for _, c := range []core.Channel{first, second} {
			if _, e = tx.AddRoute(ctx, c.ID, core.RouteInput{ConversationID: "group1", ConversationType: "group", Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"}); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `agents:
  helper: {preset: claude-default, execution_model: first-model}
  second: {preset: claude-default, execution_model: second-model, capabilities: []}
applications:
  bots:
    app-main:
      default_agent: helper
      owner_private: {enabled: true, runtime: owner-private}
      group_mention: {enabled: true}
    app-second:
      default_agent: second
      owner: {id_type: user_id, id_value: owner1}
      group_mention: {enabled: true}
`
	cfg := configFile(t, body)
	code, raw := invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 {
		t.Fatal(raw)
	}
	plan := data(t, raw)
	for _, value := range plan["blockers"].([]any) {
		b := value.(map[string]any)
		if b["code"] != "permission_expansion" {
			t.Fatalf("independent bot blocked: %v", raw)
		}
	}
	if code, raw = invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "0", "--authorize-expansion", "--reason", "enable owner permissions"); code != 0 {
		t.Fatal(raw)
	}
	for _, tt := range []struct{ name, model string }{{"bot-app-main-group", "first-model"}, {"bot-app-second-group", "second-model"}} {
		r, e := core.ReadRuntime(ctx, store.DB, tt.name)
		if e != nil {
			t.Fatal(e)
		}
		if r.ContextChannelID != "" || len(r.RouteIDs) != 1 {
			t.Fatalf("bot depends on observation: %+v", r)
		}
		policy, e := core.ResolveRuntimeTaskAgent(ctx, store.DB, r, core.RuntimeTask{RuntimeID: r.ID, RouteID: r.RouteIDs[0], Messages: []core.RuntimeMessage{{Sender: r.OwnerPrincipalID}}})
		if e != nil {
			t.Fatal(e)
		}
		if policy.ExecutionModel != tt.model || policy.BashEnabled || policy.ExternalActions != "owner_confirmation" {
			t.Fatalf("wrong bot policy: %+v", policy)
		}
	}
	r, e := core.ReadRuntime(ctx, store.DB, private.ID)
	if e != nil || !r.AgentBash || r.ExternalActions != "owner_request" || r.ExecutionModel != "first-model" {
		t.Fatalf("owner persona not applied: %+v %v", r, e)
	}
	a := &app{home: home}
	specs, e := a.unifiedSpecs(ctx, store)
	if e != nil {
		t.Fatal(e)
	}
	counts := map[string]int{}
	for _, s := range specs {
		counts[s.Kind]++
	}
	if counts["direct"] != 1 || counts["group_mention"] != 2 || counts["receiver"] != 2 || counts["source"] != 0 {
		t.Fatalf("incorrect bot service modules: %+v", counts)
	}
	// A second unchanged application must be idempotent.
	code, raw = invoke(t, home, "", "--config", cfg, "config", "plan")
	if code != 0 || data(t, raw)["ready"] != true {
		t.Fatal(raw)
	}
	applied, e := core.ReadAppliedConfig(ctx, store.DB, 0)
	if e != nil {
		t.Fatal(e)
	}
	var declaration DualModeDeclaration
	if e = json.Unmarshal(applied.Declaration, &declaration); e != nil {
		t.Fatal(e)
	}
	if len(declaration.Applications.Bots) != 2 {
		t.Fatal("bot declarations not persisted")
	}
}

func TestLegacyOwnerBindingMigratesToBotWithoutDuplicateDisabledManifest(t *testing.T) {
	home, private := dualOwnerPrivateFixture(t)
	agents := "agents:\n  helper: {preset: claude-default}\n  limited: {preset: claude-default, bash: false, capabilities: [conversation_history_read], skills: {inherit: none}}\n"
	legacy := configFile(t, agents+"applications:\n  owner_private: {enabled: true, runtime: owner-private, agent: limited}\n")
	plan := channelPlan(t, home, legacy)
	if plan["ready"] != true {
		t.Fatal(plan)
	}
	if code, raw := channelApply(t, home, legacy, plan); code != 0 {
		t.Fatal(raw)
	}
	bot := configFile(t, agents+"applications:\n  bots:\n    app-main:\n      default_agent: helper\n      owner_private: {enabled: true, runtime: owner-private, agent: limited}\n")
	plan = channelPlan(t, home, bot)
	if plan["ready"] != true {
		t.Fatal(plan)
	}
	if code, raw := channelApply(t, home, bot, plan); code != 0 {
		t.Fatal(raw)
	}
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	applied, err := core.ReadAppliedConfig(ctx, store.DB, 0)
	if err != nil {
		t.Fatal(err)
	}
	aliases := 0
	for _, o := range applied.Objects {
		if o.ObjectType == "runtime" && o.ObjectID == private.ID {
			aliases++
			if o.Name != "bot:app-main:owner" {
				t.Fatalf("old application alias survived: %+v", o)
			}
		}
	}
	if aliases != 1 {
		t.Fatalf("expected exactly one owner binding, got %d", aliases)
	}
	a := &app{home: home}
	specs, err := a.unifiedSpecs(ctx, store)
	if err != nil || len(specs) != 2 {
		t.Fatalf("owner service disabled by old manifest: %+v %v", specs, err)
	}
	disabled := configFile(t, agents+"applications: {}\n")
	plan = channelPlan(t, home, disabled)
	if code, raw := channelApply(t, home, disabled, plan); code != 0 {
		t.Fatal(raw)
	}
	specs, err = a.unifiedSpecs(ctx, store)
	if err != nil || len(specs) != 0 {
		t.Fatalf("removed bot worker still selected: %+v %v", specs, err)
	}
	current, err := core.ReadRuntime(ctx, store.DB, private.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = core.ResolveRuntimeTaskAgent(ctx, store.DB, current, core.RuntimeTask{RuntimeID: current.ID, RouteID: current.RouteIDs[0]}); core.ErrorCode(err) != "denied" {
		t.Fatalf("removed bot recovered unmanaged privileges: %v", err)
	}
}

func TestProactivePlanAndApplyNeedNoRobotOrSendCapability(t *testing.T) {
	home, _ := dualOwnerPrivateFixture(t)
	body := `data_sources:
  watched: {channel: dws-main}
applications:
  proactive:
    enabled: true
    source: watched
    owner: {id_type: user_id, id_value: owner1}
`
	cfg := configFile(t, body)
	plan := channelPlan(t, home, cfg)
	for _, value := range plan["blockers"].([]any) {
		if value.(map[string]any)["code"] != "permission_expansion" {
			t.Fatalf("observation unexpectedly needs robot delivery: %+v", plan)
		}
	}
	if code, raw := invoke(t, home, "", "--config", cfg, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", "0", "--authorize-expansion", "--reason", "delegate background work"); code != 0 {
		t.Fatal(raw)
	}
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source, err := core.ReadDataSource(ctx, store.DB, "watched")
	if err != nil || source.MemberRobotCode != "" {
		t.Fatalf("source requires robot membership: %+v %v", source, err)
	}
	_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.independent.discovery"}, func(tx *core.Tx) (any, error) {
		channel, e := core.ReadChannel(ctx, tx.Conn, source.ChannelID)
		if e != nil {
			return nil, e
		}
		groups := []core.DataSourceGroup{{ID: "observed", Name: "Observed"}}
		if _, e = tx.SyncDataSourceGroupDetails(ctx, source.ID, groups); e != nil {
			return nil, e
		}
		if e = tx.RecordSourceGroupDiscovery(ctx, source.ID, source.Version, channel.ConfigVersion, groups); e != nil {
			return nil, e
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	plan = channelPlan(t, home, cfg)
	if plan["ready"] != true {
		t.Fatal(plan)
	}
	if code, raw := channelApply(t, home, cfg, plan); code != 0 {
		t.Fatal(raw)
	}
	r, err := core.ReadRuntime(ctx, store.DB, "proactive")
	if err != nil || r.DeliveryRouteID != "" || !r.AgentBash || r.ExternalActions != "owner_delegated" {
		t.Fatalf("incorrect background runtime: %+v %v", r, err)
	}
	policy, err := core.ResolveRuntimeTaskAgent(ctx, store.DB, r, core.RuntimeTask{RuntimeID: r.ID, RouteID: r.RouteIDs[0]})
	if err != nil || policy.ExternalActions != "owner_delegated" || policy.Skills.Inherit != "executor" {
		t.Fatalf("background agent is not fully equipped: %+v %v", policy, err)
	}
}

func TestLegacyOwnerCannotBeShadowedByBotDeclaration(t *testing.T) {
	home, _ := dualOwnerPrivateFixture(t)
	cfg := configFile(t, `agents:
  helper: {preset: claude-default}
  limited: {preset: claude-default, bash: false, capabilities: [conversation_history_read]}
applications:
  owner_private: {enabled: true, runtime: owner-private, agent: limited}
  bots:
    app-main:
      default_agent: helper
      group_mention: {enabled: false}
`)
	plan := channelPlan(t, home, cfg)
	found := false
	for _, value := range plan["blockers"].([]any) {
		found = found || value.(map[string]any)["code"] == "application_declaration_conflict"
	}
	if !found {
		t.Fatalf("bot silently shadowed the legacy owner application: %+v", plan)
	}
}

func TestDelegatedOwnerRequiresExactDWSUserID(t *testing.T) {
	home, _ := dualOwnerPrivateFixture(t)
	for _, owner := range []string{
		"{id_type: staff_id, id_value: owner1}",
		"{id_type: user_id, id_value: someone-else}",
	} {
		cfg := configFile(t, "data_sources:\n  watched: {channel: dws-main}\napplications:\n  proactive:\n    enabled: true\n    source: watched\n    owner: "+owner+"\n")
		plan := channelPlan(t, home, cfg)
		found := false
		for _, value := range plan["blockers"].([]any) {
			found = found || value.(map[string]any)["code"] == "delegated_owner_unverified"
		}
		if !found {
			t.Fatalf("delegation accepted a non-profile owner %s: %+v", owner, plan)
		}
	}
}
