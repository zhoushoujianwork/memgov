package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRuntimeAgentResolvesExactGroupBindingAndLivePolicy(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	var app Channel
	var one, two Route
	var cfg RuntimeConfig
	runtimeMutate(t, f.s, "test.group.context", func(tx *Tx) (any, error) {
		return tx.AddRoute(ctx, f.channel.ID, RouteInput{ConversationID: "cid:second", ConversationType: "group"})
	})
	runtimeMutate(t, f.s, "test.group.app", func(tx *Tx) (any, error) {
		var e error
		app, e = tx.AddChannel(ctx, ChannelInput{Name: "app-policy", Kind: ChannelDingTalkApp, Identity: ChannelIdentity{ExpectedCorpID: f.channel.Tenant, ClientID: "client", RobotCode: "bot", HistoryChannel: f.channel.ID}})
		return app, e
	})
	runtimeMutate(t, f.s, "test.group.routes", func(tx *Tx) (any, error) {
		var e error
		one, e = tx.AddRoute(ctx, app.ID, RouteInput{ConversationID: f.watch.ConversationID, ConversationType: "group", Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		if e != nil {
			return nil, e
		}
		two, e = tx.AddRoute(ctx, app.ID, RouteInput{ConversationID: "cid:second", ConversationType: "group", Mode: "assistant", Triggers: []string{"mention"}, AudiencePolicy: "conversation", SendPolicy: "reply_to_trigger"})
		return two, e
	})
	runtimeMutate(t, f.s, "test.group.configure", func(tx *Tx) (any, error) {
		var e error
		cfg, e = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "group-policy", Channel: app.ID, RouteIDs: []string{one.ID, two.ID}, DeliveryRouteID: one.ID, Owner: f.owner, ApplicationMode: "group_mention", ContextChannel: f.channel.ID})
		return cfg, e
	})
	policies := map[string]RuntimeAgentPolicy{"default": {Preset: "default-preset", MemoryScope: "conversation_published", ExternalActions: "owner_confirmation", Capabilities: []string{}}, "special": {Preset: "special-preset", ClaudeProfile: "cc", ExecutionModel: "profile", MemoryScope: "conversation_published", ExternalActions: "owner_confirmation", Capabilities: []string{"local_read"}, Directories: []string{"/explicit/group/docs"}}}
	apply := func(expected int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"agents": policies, "applications": map[string]any{"group_mention": map[string]any{"enabled": true, "default_agent": "default", "bindings": []map[string]string{{"conversation_id": one.ConversationID, "agent": "special"}}}}})
		runtimeMutate(t, f.s, "test.group.apply", func(tx *Tx) (any, error) {
			return tx.CommitAppliedConfig(ctx, expected, 1, body, []ManagedConfigObject{{Kind: "application", Name: "group_mention", ObjectType: "runtime", ObjectID: cfg.ID}})
		})
	}
	apply(0)
	for _, tt := range []struct{ route, agent, preset string }{{one.ID, "special", "special-preset"}, {two.ID, "default", "default-preset"}} {
		p, err := ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, RuntimeTask{RuntimeID: cfg.ID, RouteID: tt.route})
		if err != nil || p.Agent != tt.agent || p.Preset != tt.preset {
			t.Fatalf("policy %+v %v", p, err)
		}
	}
	special := policies["special"]
	special.BashEnabled, special.Directories = true, nil
	policies["special"] = special
	apply(1)
	for _, routeID := range []string{one.ID, two.ID} {
		p, err := ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, RuntimeTask{RuntimeID: cfg.ID, RouteID: routeID})
		if err != nil || p.BashEnabled != (routeID == one.ID) {
			t.Fatalf("Bash binding leaked across groups: %+v %v", p, err)
		}
	}
	special.ExternalActions = "owner_request"
	policies["special"] = special
	apply(2)
	if _, err := ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, RuntimeTask{RuntimeID: cfg.ID, RouteID: one.ID}); ErrorCode(err) != "denied" {
		t.Fatalf("group acquired owner_request: %v", err)
	}
	special.ExternalActions = "owner_confirmation"
	policies["special"] = special
	bad := policies["special"]
	bad.MemoryScope = "owner_authorized"
	policies["special"] = bad
	apply(3)
	if _, err := ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, RuntimeTask{RuntimeID: cfg.ID, RouteID: one.ID}); ErrorCode(err) != "denied" {
		t.Fatalf("owner memory escaped: %v", err)
	}
	runtimeMutate(t, f.s, "test.route.ignore", func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, two.ID, two.Version, RouteInput{Mode: "ignore"}, "exclude")
	})
	if _, err := ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, RuntimeTask{RuntimeID: cfg.ID, RouteID: two.ID}); ErrorCode(err) != "denied" {
		t.Fatalf("ignored route resolved: %v", err)
	}
}
func TestRuntimeAttemptRecordsActualAgentAndFencesPolicyChange(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	task := createRuntimeTask(t, f, "agent-audit")
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "test.claim", func(tx *Tx) (any, error) {
		var e error
		task, attempt, e = tx.ClaimRuntimeTask(ctx, f.config.ID, "", "default-model", "old-preset", "old-commit", "")
		return attempt, e
	})
	policy, err := ResolveRuntimeTaskAgent(ctx, f.s.DB, f.config, task)
	if err != nil {
		t.Fatal(err)
	}
	runtimeMutate(t, f.s, "test.policy.audit", func(tx *Tx) (any, error) {
		return nil, tx.RecordRuntimeAttemptAgent(ctx, f.config, task, attempt.ID, policy, "verified-commit")
	})
	var preset, commit, model string
	err = f.s.DB.QueryRow("SELECT preset_name,preset_commit,model FROM runtime_attempts WHERE id=?", attempt.ID).Scan(&preset, &commit, &model)
	if err != nil || preset != policy.Preset || commit != "verified-commit" || model != policy.ExecutionModel {
		t.Fatalf("actual agent %s %s %s %v", preset, commit, model, err)
	}
	runtimeMutate(t, f.s, "test.invalidate", func(tx *Tx) (any, error) { return nil, tx.InvalidateRuntimeConfigWork(ctx, f.config.ID) })
	_, err = f.s.Mutate(ctx, Request{Command: "test.stale.policy", Actor: time.Now().String()}, func(tx *Tx) (any, error) {
		return nil, tx.RecordRuntimeAttemptAgent(ctx, f.config, task, attempt.ID, policy, "verified-commit")
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("stale policy record accepted: %v", err)
	}
}
