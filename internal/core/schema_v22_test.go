package core

import (
	"context"
	"testing"
)

func legacyBackgroundOutbox(t *testing.T, f runtimeFixture, task RuntimeTask, state string) OutboxView {
	t.Helper()
	id := NewID()
	digest := Digest(map[string]any{"task": task.ID, "version": task.Version, "result": "legacy answer"})
	// V21 could prepare these automatic owner notifications. V22 must reject
	// their dispatch even if a stale queue is restored after migration.
	_, err := f.s.DB.Exec(`INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,sender_identity,transport,content,input_digest,display_digest,send_policy,state,reason,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, f.channel.ID, f.direct.ID, f.direct.Version, task.ID, f.direct.ConversationID, f.direct.AudienceKey, f.channel.Identity.ExpectedUserID, "bot_dm", "legacy answer", digest+id, digest+id, "dispatch_only", state, "result", Now(), Now())
	if err != nil {
		t.Fatal(err)
	}
	return OutboxView{ID: id, State: state}
}

func TestSchema22StopsOnlyLegacyBackgroundNotifications(t *testing.T) {
	for _, mode := range []string{"proactive", "direct"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntimeFixture(t, 1, 300)
			task := createRuntimeTask(t, f, "migration")
			if _, err := f.s.DB.Exec("UPDATE runtime_tasks SET status='completed',result='kept conclusion' WHERE id=?", task.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.DB.Exec("UPDATE runtime_configs SET application_mode=?,agent_bash=0,external_actions='owner_confirmation' WHERE id=?", mode, f.config.ID); err != nil {
				t.Fatal(err)
			}
			rows := map[string]OutboxView{}
			for _, state := range []string{"ready", "sending", "accepted", "failed", "unknown"} {
				rows[state] = legacyBackgroundOutbox(t, f, task, state)
			}
			dropSchema23(t, f.s)
			for _, statement := range []string{"DROP TABLE runtime_message_actions", "DELETE FROM schema_migrations WHERE version=22"} {
				if _, err := f.s.DB.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			path := f.s.Path
			f.s.Close()
			ctx := context.Background()
			if _, err := Open(ctx, path, false); ErrorCode(err) != "invalid_input" {
				t.Fatalf("implicit migration: %v", err)
			}
			upgraded, err := Open(ctx, path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			f.s = upgraded
			for old, row := range rows {
				want := old
				if mode == "proactive" && old == "ready" {
					want = "stale"
				}
				if mode == "proactive" && old == "sending" {
					want = "unknown"
				}
				var got string
				if err := f.s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", row.ID).Scan(&got); err != nil || got != want {
					t.Fatalf("%s changed to %s, want %s: %v", old, got, want, err)
				}
			}
			var result string
			if err := f.s.DB.QueryRow("SELECT result FROM runtime_tasks WHERE id=?", task.ID).Scan(&result); err != nil || result != "kept conclusion" {
				t.Fatalf("lost completed work: %q %v", result, err)
			}
			cfg, err := ReadRuntime(ctx, f.s.DB, f.config.ID)
			if err != nil || cfg.AgentBash || cfg.ExternalActions != "owner_confirmation" {
				t.Fatalf("migration expanded execution permission: %+v %v", cfg, err)
			}
			if mode == "proactive" {
				row := rows["ready"]
				if _, err := f.s.DB.Exec("UPDATE outbox SET state='ready' WHERE id=?", row.ID); err != nil {
					t.Fatal(err)
				}
				_, err := f.s.Mutate(ctx, Request{Scope: "global", Command: "legacy.begin"}, func(tx *Tx) (any, error) { return tx.BeginDelivery(ctx, row.ID) })
				if ErrorCode(err) != "denied" {
					t.Fatalf("legacy automatic delivery resumed: %v", err)
				}
				preview, err := PreviewDraft(ctx, f.s.DB, row.ID)
				if err != nil || preview.Sendable {
					t.Fatalf("legacy dispatch preview: %+v %v", preview, err)
				}
				_, err = f.s.Mutate(ctx, Request{Scope: "global", Command: "legacy.dispatch"}, func(tx *Tx) (any, error) { return tx.AuthorizeDispatch(ctx, row.ID, preview.DisplayDigest) })
				if ErrorCode(err) != "denied" {
					t.Fatalf("legacy hand dispatch resumed: %v", err)
				}
			}
		})
	}
}
