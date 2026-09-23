package core

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Reconstruct only removed objects for tests of historical migrations. Production
// never downgrades a database; its old state is retained in a sealed archive.
func dropSchema27(t *testing.T, s *Store) {
	t.Helper()
	for _, name := range []string{"migration_runs", "migration_sources", "candidates", "reviews", "memories", "revisions", "memory_evidence", "relations", "jobs", "plans", "purge_runs"} {
		sql := regexp.MustCompile(`(?s)CREATE TABLE ` + name + `\s*\(.*?\);`).FindString(schema)
		if sql == "" {
			t.Fatal("missing legacy table", name)
		}
		if _, err := s.DB.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, sql := range []string{
		"ALTER TABLE migration_runs ADD COLUMN purpose TEXT NOT NULL DEFAULT 'migration'",
		"ALTER TABLE migration_runs ADD COLUMN route_id TEXT NOT NULL DEFAULT ''",
		regexp.MustCompile(`(?s)CREATE TABLE memory_publications\(.*?\);`).FindString(schemaV2),
		"ALTER TABLE runtime_tasks ADD COLUMN candidate_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE runtime_tasks ADD COLUMN memory_status TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE runtime_tasks ADD COLUMN memory_error_code TEXT NOT NULL DEFAULT ''",
		regexp.MustCompile(`(?s)CREATE TABLE runtime_reviews \(.*?\);`).FindString(schemaV25),
		"CREATE INDEX runtime_review_queue ON runtime_reviews(status,created_at)",
		"CREATE INDEX memories_scope ON memories(workspace_id,status)",
		"DELETE FROM schema_migrations WHERE version=27",
	} {
		if _, err := s.DB.Exec(sql); err != nil {
			t.Fatal(sql, err)
		}
	}
	for _, name := range []string{"memory_insert", "memory_update", "memory_delete"} {
		sql := regexp.MustCompile(`(?s)CREATE TRIGGER ` + name + ` .*?END;`).FindString(schema)
		if _, err := s.DB.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceUpgradeArchivesAndFencesLegacyWork(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "init", true: "service"}[managed], func(t *testing.T) {
			ctx := context.Background()
			f := newRuntimeFixture(t, 1, 30)
			task := createRuntimeTask(t, f, "old-memory")
			var attempt RuntimeAttempt
			runtimeMutate(t, f.s, "claim", func(tx *Tx) (any, error) {
				var e error
				_, attempt, e = tx.ClaimRuntimeTask(ctx, f.config.ID, "", "", "", "", "")
				return nil, e
			})
			if attempt.ID == "" {
				t.Fatal("missing attempt")
			}
			dropSchema27(t, f.s)
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := f.s.DB.Exec(query, args...); err != nil {
					t.Fatal(query, err)
				}
			}
			exec("INSERT INTO memories VALUES('old-memory','global','fact','historical title','historical summary','historical knowledge','active',1,'{}',?)", Now())
			exec("UPDATE runtime_tasks SET kind='memory',candidate_id='old-candidate',memory_status='reviewing' WHERE id=?", task.ID)
			exec("INSERT INTO runtime_reviews(id,task_id,task_version,runtime_id,source_attempt_id,candidate_input,deadline_at,created_at) VALUES('review',?,?,?,?, 'null',?,?)", task.ID, task.Version, f.config.ID, attempt.ID, Now(), Now())
			exec("UPDATE runtime_work_leases SET phase='review' WHERE id=?", attempt.ID)
			exec("INSERT INTO runtime_resource_locks(lease_id,root) VALUES(?,?)", attempt.ID, t.TempDir())
			for _, state := range []string{"pending", "confirmed", "executing"} {
				exec("INSERT INTO runtime_pending_actions(id,task_id,task_version,kind,target,payload,payload_digest,status,created_at,updated_at) VALUES(?,?,?,'git_push','origin','old action','old-digest',?,?,?)", "action-"+state, task.ID, task.Version, state, Now(), Now())
			}
			for _, state := range []string{"draft", "ready", "sending", "sent", "unknown"} {
				exec("INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,content,citations,input_digest,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", "out-"+state, f.channel.ID, f.direct.ID, f.direct.Version, task.ID, f.direct.ConversationID, f.direct.AudienceKey, "old reply", `["old-memory"]`, state, state, Now(), Now())
			}
			exec("INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,content,citations,input_digest,state,created_at,updated_at) VALUES('task-only',?,?,?,?,?,?,'old reply','[]','task-only','ready',?,?)", f.channel.ID, f.direct.ID, f.direct.Version, task.ID, f.direct.ConversationID, f.direct.AudienceKey, Now(), Now())
			for _, command := range []string{"memory.retire", "memgov memory retire", "runtime.review.complete", "memgov source ingest", "job.submit", "outbox.dispatch", "memgov channel add", "message.intake"} {
				exec("INSERT INTO idempotency VALUES('global',?,'keep-or-remove','request','{}',?)", command, Now())
			}
			path := f.s.Path
			f.s.Close()
			var err error
			if managed {
				_, err = PrepareServiceDatabase(ctx, path)
			} else {
				var store *Store
				store, err = Open(ctx, path, true)
				if err == nil {
					store.Close()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			archives, err := filepath.Glob(filepath.Join(filepath.Dir(path), "backups", "service-upgrades", "*.db"))
			if err != nil || len(archives) != 1 {
				t.Fatal(archives, err)
			}
			backup, err := VerifyBackup(ctx, archives[0])
			if err != nil || backup.SchemaVersion != 26 {
				t.Fatal(backup, err)
			}
			archive, err := sql.Open("sqlite", "file:"+archives[0]+"?mode=ro&immutable=1")
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			var content string
			if err = archive.QueryRow("SELECT content FROM memories WHERE id='old-memory'").Scan(&content); err != nil || content != "historical knowledge" {
				t.Fatal(content, err)
			}
			active, err := Open(ctx, path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer active.Close()
			for _, table := range []string{"memories", "candidates", "reviews", "runtime_reviews", "memory_publications", "migration_runs", "jobs", "plans"} {
				var count int
				if err = active.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE name=?", table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("old table %s survived: %d %v", table, count, err)
				}
			}
			for command, want := range map[string]int{"memory.retire": 0, "memgov memory retire": 0, "runtime.review.complete": 0, "memgov source ingest": 0, "job.submit": 0, "outbox.dispatch": 1, "memgov channel add": 1, "message.intake": 1} {
				var count int
				if err = active.DB.QueryRow("SELECT count(*) FROM idempotency WHERE command=?", command).Scan(&count); err != nil || count != want {
					t.Fatalf("cache %s: count %d want %d: %v", command, count, want, err)
				}
			}
			got, err := ReadRuntimeTask(ctx, active.DB, task.ID)
			if err != nil || got.Status != "cancelled" || got.ErrorCode != "memory_system_removed" {
				t.Fatal(got, err)
			}
			if _, err = active.Mutate(ctx, Request{}, func(tx *Tx) (any, error) { return tx.SetRuntimeTaskStatus(ctx, task.ID, "pending") }); ErrorCode(err) != "denied" {
				t.Fatal("legacy retry", err)
			}
			var locks, released int
			if err = active.DB.QueryRow("SELECT count(*) FROM runtime_resource_locks").Scan(&locks); err != nil || locks != 0 {
				t.Fatal(locks, err)
			}
			if err = active.DB.QueryRow("SELECT released FROM runtime_work_leases WHERE id=?", attempt.ID).Scan(&released); err != nil || released != 1 {
				t.Fatal(released, err)
			}
			for id, want := range map[string]string{"action-pending": "stale", "action-confirmed": "stale", "action-executing": "unknown"} {
				var state string
				if err = active.DB.QueryRow("SELECT status FROM runtime_pending_actions WHERE id=?", id).Scan(&state); err != nil || state != want {
					t.Fatal(id, state, want, err)
				}
			}
			for id, want := range map[string]string{"out-draft": "stale", "out-ready": "stale", "out-sending": "unknown", "out-sent": "sent", "out-unknown": "unknown", "task-only": "stale"} {
				var state string
				if err = active.DB.QueryRow("SELECT state FROM outbox WHERE id=?", id).Scan(&state); err != nil || state != want {
					t.Fatal(id, state, want, err)
				}
			}
			preview, err := PreviewDraft(ctx, active.DB, "out-sent")
			if err != nil || preview.Sendable || len(preview.Citations) != 1 || !preview.Citations[0].Unknown {
				t.Fatal(preview, err)
			}
			report, err := active.Doctor(ctx)
			if err != nil || report["healthy"] != true {
				t.Fatal(report, err)
			}
			if _, err = RestoreBackup(ctx, path, archives[0], backup.SHA256, Request{}); err == nil {
				t.Fatal("old knowledge archive accepted as current operational backup")
			}
		})
	}
}

func TestInitArchiveFailureLeavesOldSchemaIntact(t *testing.T) {
	s := testStore(t)
	dropSchema27(t, s)
	path := s.Path
	s.Close()
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "backups"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), path, true); err == nil || !strings.Contains(err.Error(), "archive failed") {
		t.Fatal(err)
	}
	old, err := OpenReadCompatible(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var version int
	if err = old.DB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != 26 {
		t.Fatal(version, err)
	}
	var count int
	if err = old.DB.QueryRow("SELECT count(*) FROM memories").Scan(&count); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceUpgradeRollsBackAllChangesOnFailure(t *testing.T) {
	s := testStore(t)
	dropSchema27(t, s)
	if _, err := s.DB.Exec("INSERT INTO memories VALUES('old','global','fact','title','summary','content','active',1,'{}',?)", Now()); err != nil {
		t.Fatal(err)
	}
	// An invalid declaration is detected after the DROP statements, inside the
	// same migration transaction, so failure must restore the complete old domain.
	if _, err := s.DB.Exec("INSERT INTO applied_configs VALUES('old',1,1,'invalid-json','[]','digest',?)", Now()); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	s.Close()
	if _, err := Open(context.Background(), path, true); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	old, err := OpenReadCompatible(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var count int
	if err = old.DB.QueryRow("SELECT count(*) FROM memories WHERE id='old'").Scan(&count); err != nil || count != 1 {
		t.Fatal("partial destructive migration", count, err)
	}
	archives, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "backups", "service-upgrades", "*.db"))
	if len(archives) != 1 {
		t.Fatal("failed migration lost archive", archives)
	}
}

func TestUpgradeArchivesConfigAndAllLegacyHomes(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "init", true: "service"}[managed], func(t *testing.T) {
			s := testStore(t)
			dropSchema27(t, s)
			home := filepath.Dir(s.Path)
			defaultHome := filepath.Join(home, "agent-homes", "owner")
			relativeHome := filepath.Join(home, "custom-owner")
			appliedHome := t.TempDir()
			for _, dir := range []string{defaultHome, relativeHome, appliedHome} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("legacy knowledge "+dir), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw := []byte("agents:\n  owner:\n    home: custom-owner\n")
			if err := os.WriteFile(filepath.Join(home, "config.yaml"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			declaration := JSON(map[string]any{"agents": map[string]any{"former": map[string]any{"home": appliedHome}}})
			if _, err := s.DB.Exec("INSERT INTO applied_configs VALUES('old',1,1,?,'[]','digest',?)", declaration, Now()); err != nil {
				t.Fatal(err)
			}
			path := s.Path
			s.Close()
			if managed {
				if _, err := PrepareServiceDatabase(context.Background(), path); err != nil {
					t.Fatal(err)
				}
			} else {
				store, err := Open(context.Background(), path, true)
				if err != nil {
					t.Fatal(err)
				}
				store.Close()
			}
			archives, _ := filepath.Glob(filepath.Join(home, "backups", "service-upgrades", "*.knowledge.tar"))
			if len(archives) != 1 {
				t.Fatal(archives)
			}
			file, err := os.Open(archives[0])
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got := map[string]string{}
			reader := tar.NewReader(file)
			for {
				h, err := reader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if h.Typeflag == tar.TypeReg {
					data, err := io.ReadAll(reader)
					if err != nil {
						t.Fatal(err)
					}
					got[h.Name] = string(data)
				}
			}
			if got["config.yaml"] != string(raw) || got["agent-homes/owner/CLAUDE.md"] != "legacy knowledge "+defaultHome || got["custom-agent-homes/"+Hash([]byte(relativeHome))+"/CLAUDE.md"] != "legacy knowledge "+relativeHome || got["custom-agent-homes/"+Hash([]byte(appliedHome))+"/CLAUDE.md"] != "legacy knowledge "+appliedHome {
				t.Fatal("incomplete legacy knowledge archive", got)
			}
			info, err := VerifyBackup(context.Background(), strings.TrimSuffix(archives[0], ".knowledge.tar"))
			if err != nil || info.KnowledgePath != archives[0] || info.KnowledgeSHA256 == "" {
				t.Fatal(info, err)
			}
			var manifest map[string]any
			if err = json.Unmarshal([]byte(got["archive-manifest.json"]), &manifest); err != nil || manifest["database_sha256"] != info.SHA256 {
				t.Fatal("unbound archive manifest", manifest, err)
			}
			if err = verifyArchiveMetadata(info); err != nil {
				t.Fatal("archive checksum metadata missing", err)
			}
			archiveBytes, err := os.ReadFile(archives[0])
			if err != nil {
				t.Fatal(err)
			}
			for name, contents := range map[string][]byte{
				"corrupt-file":      bytes.Replace(archiveBytes, []byte("legacy knowledge"), []byte("broken knowledge"), 1),
				"truncated-trailer": archiveBytes[:len(archiveBytes)-512],
			} {
				path := filepath.Join(t.TempDir(), name+".tar")
				if err = os.WriteFile(path, contents, 0600); err != nil {
					t.Fatal(err)
				}
				if err = verifyLegacyKnowledgeArchive(context.Background(), path, info.SHA256, nil); err == nil {
					t.Fatal("invalid archive verified", name)
				}
			}
			if err = verifyLegacyKnowledgeArchive(context.Background(), archives[0], "wrong-db-hash", nil); err == nil {
				t.Fatal("archive accepted a different database")
			}
		})
	}
}

func TestUpgradeRefusesIncompleteLegacyFileArchive(t *testing.T) {
	s := testStore(t)
	dropSchema27(t, s)
	home := filepath.Dir(s.Path)
	if err := os.MkdirAll(filepath.Join(home, "agent-homes", "owner"), 0700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(external, []byte("never follow"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(home, "agent-homes", "owner", "linked.md")); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	s.Close()
	if _, err := Open(context.Background(), path, true); err == nil || !strings.Contains(err.Error(), "legacy knowledge archive failed") {
		t.Fatal("incomplete archive allowed destructive migration", err)
	}
	old, err := OpenReadCompatible(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var version int
	if err = old.DB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != 26 {
		t.Fatal(version, err)
	}
}
