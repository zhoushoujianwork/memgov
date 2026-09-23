package core

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRuntimeBashDefaultsAndExplicitDisable(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	if !f.config.AgentBash || f.config.ExternalActions != "owner_delegated" {
		t.Fatalf("proactive owner capability missing: %+v", f.config)
	}
	for _, mode := range []string{"direct", "proactive", "group_mention"} {
		for _, explicit := range []bool{false, true} {
			in := RuntimeConfigInput{Name: "defaults", RouteIDs: f.config.RouteIDs, DeliveryRouteID: f.direct.ID, Owner: f.owner, ApplicationMode: mode}
			if explicit {
				disabled := false
				in.AgentBash = &disabled
			}
			if err := normalizeRuntimeInput(&in); err != nil {
				t.Fatal(err)
			}
			want := mode != "group_mention" && !explicit
			if *in.AgentBash != want || (in.ExternalActions == "owner_request") != (mode == "direct" && !explicit) || (in.ExternalActions == "owner_delegated") != (mode == "proactive") {
				t.Fatalf("mode=%s explicit=%t policy=%+v", mode, explicit, in)
			}
		}
	}
}

func TestOwnerPrivatePolicyIndependentFromProactiveAndRejectsWrongIdentity(t *testing.T) {
	f, app, inbox, _ := crossConfirmationFixture(t)
	ctx := context.Background()
	var cfg RuntimeConfig
	runtimeMutate(t, f.s, "test.owner.configure", func(tx *Tx) (any, error) {
		inbox.ConversationID = f.owner.IDValue
		if _, err := tx.Conn.ExecContext(ctx, "UPDATE channel_routes SET conversation_id=? WHERE id=?", inbox.ConversationID, inbox.ID); err != nil {
			return nil, err
		}
		var err error
		cfg, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "owner-private", Channel: app.ID, RouteIDs: []string{inbox.ID}, DeliveryRouteID: inbox.ID, Owner: f.owner, ApplicationMode: "direct"})
		return cfg, err
	})
	task := RuntimeTask{RuntimeID: cfg.ID, RouteID: inbox.ID}
	p, err := ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, task)
	if err != nil || !p.BashEnabled || p.ExternalActions != "owner_request" || p.Managed {
		t.Fatalf("unconfigured owner default: %+v %v", p, err)
	}
	apply := func(version int, ownerAgent string, agents map[string]RuntimeAgentPolicy) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"agents": agents, "applications": map[string]any{
			"owner_private": map[string]any{"enabled": true, "runtime": cfg.Name, "agent": ownerAgent},
			"proactive":     map[string]any{"enabled": true, "agent": "proactive"},
		}})
		runtimeMutate(t, f.s, "test.owner.apply", func(tx *Tx) (any, error) {
			return tx.CommitAppliedConfig(ctx, version, 1, body, []ManagedConfigObject{{Kind: "application", Name: "owner_private", ObjectType: "runtime", ObjectID: cfg.ID}})
		})
	}
	agents := map[string]RuntimeAgentPolicy{
		"owner":     {Preset: "owner-preset", BashEnabled: true, ExternalActions: "owner_request"},
		"proactive": {Preset: "proactive-preset", ExternalActions: "owner_confirmation"},
	}
	apply(0, "owner", agents)
	p, err = ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, task)
	if err != nil || p.Preset != "owner-preset" || !p.BashEnabled || !p.Managed {
		t.Fatalf("owner used another Agent: %+v %v", p, err)
	}
	on := p
	owner := agents["owner"]
	owner.BashEnabled, owner.ExternalActions = false, "owner_confirmation"
	agents["owner"] = owner
	apply(1, "owner", agents)
	p, err = ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, task)
	if err != nil || p.BashEnabled || Digest(on) == Digest(p) {
		t.Fatalf("policy disable did not change fingerprint: %+v %v", p, err)
	}
	apply(2, "", agents)
	p, err = ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, task)
	if err != nil || !p.BashEnabled || p.ExternalActions != "owner_request" {
		t.Fatalf("built-in owner lost: %+v %v", p, err)
	}
	runtimeMutate(t, f.s, "test.owner.revoke", func(tx *Tx) (any, error) {
		return tx.Conn.ExecContext(ctx, "UPDATE identity_aliases SET verified=0 WHERE principal_id=?", cfg.OwnerPrincipalID)
	})
	if _, err := ResolveRuntimeTaskAgent(ctx, f.s.DB, cfg, task); ErrorCode(err) != "denied" {
		t.Fatalf("revoked owner got Bash: %v", err)
	}
}

func TestFullBashConfigurationRejectsWrongOwnerAndGroupOwnerRequest(t *testing.T) {
	f, app, inbox, _ := crossConfirmationFixture(t)
	_, err := f.s.Mutate(context.Background(), Request{Scope: "global", Command: "test.owner.wrong"}, func(tx *Tx) (any, error) {
		return tx.ConfigureRuntime(context.Background(), RuntimeConfigInput{Name: "wrong-owner", Channel: app.ID, RouteIDs: []string{inbox.ID}, DeliveryRouteID: inbox.ID, Owner: f.owner, ApplicationMode: "direct"})
	})
	if ErrorCode(err) != "denied" {
		t.Fatalf("mismatched owner dispatch address accepted: %v", err)
	}
	for _, mode := range []string{"proactive", "group_mention"} {
		in := RuntimeConfigInput{Name: "wrong-mode", RouteIDs: f.config.RouteIDs, DeliveryRouteID: f.direct.ID, Owner: f.owner, ApplicationMode: mode, ExternalActions: "owner_request"}
		if err := normalizeRuntimeInput(&in); ErrorCode(err) != "denied" {
			t.Fatalf("owner_request accepted for %s: %v", mode, err)
		}
	}
}

func TestSchemaNineteenPolicyMigrationAndReopen(t *testing.T) {
	f := newRuntimeFixture(t, 1, 300)
	ctx := context.Background()
	var other RuntimeConfig
	runtimeMutate(t, f.s, "test.migration.second", func(tx *Tx) (any, error) {
		var err error
		other, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "another-watcher", Channel: f.channel.ID, RouteIDs: []string{f.watch.ID}, DeliveryRouteID: f.direct.ID, Owner: f.owner})
		return other, err
	})
	// Reconstruct the released v18 columns and ledger. Route admission remains
	dropSchema23(t, f.s)
	// independently checked after migration; a mode value alone grants no access.
	if _, err := f.s.DB.Exec("UPDATE runtime_configs SET application_mode='direct' WHERE id=?", f.config.ID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"DROP TABLE runtime_message_actions",
		"DROP TABLE direct_conversation_contacts",
		"ALTER TABLE data_sources DROP COLUMN retention_error_code",
		"ALTER TABLE data_sources DROP COLUMN last_retention_count",
		"ALTER TABLE data_sources DROP COLUMN last_retention_at",
		"ALTER TABLE data_sources DROP COLUMN last_direct_received_at",
		"ALTER TABLE data_sources DROP COLUMN direct_discovery_covered_until",
		"ALTER TABLE data_sources DROP COLUMN retention_days",
		"ALTER TABLE data_sources DROP COLUMN backfill_after_enable",
		"ALTER TABLE data_sources DROP COLUMN direct_enabled_at",
		"ALTER TABLE data_sources DROP COLUMN direct_enabled",
		"ALTER TABLE coverage_windows DROP COLUMN resolved_at",
		"ALTER TABLE runtime_configs DROP COLUMN agent_bash", "ALTER TABLE runtime_configs DROP COLUMN external_actions",
		"ALTER TABLE runtime_tasks DROP COLUMN resume",
		"ALTER TABLE runtime_attempts DROP COLUMN agent_session",
		"DELETE FROM schema_migrations WHERE version IN (19,20,21,22)",
	} {
		if _, err := f.s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	path := f.s.Path
	f.s.Close()
	if _, err := Open(ctx, path, false); ErrorCode(err) != "invalid_input" {
		t.Fatalf("migration should require init: %v", err)
	}
	s, err := Open(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{f.config.ID, other.ID} {
		c, err := ReadRuntime(ctx, s.DB, id)
		want := id == f.config.ID
		if err != nil || c.AgentBash != want || (c.ExternalActions == "owner_request") != want {
			t.Fatalf("migration/reopen lost policy: %+v %v", c, err)
		}
	}
	c, _ := ReadRuntime(ctx, s.DB, f.config.ID)
	if _, err := ResolveRuntimeTaskAgent(ctx, s.DB, c, RuntimeTask{RuntimeID: c.ID, RouteID: f.watch.ID}); ErrorCode(err) != "denied" {
		t.Fatalf("migration admitted a non-owner route: %v", err)
	}
}
