package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// createV1Database writes a database exactly as the v1 binary did: baseline
// schema.sql plus a single ledger row, with no v2 objects.
func createV1Database(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{schema,
		"INSERT INTO schema_migrations VALUES(1,'" + Hash([]byte(schema)) + "','" + Now() + "')",
		"INSERT INTO settings VALUES('role','authoritative')",
		"INSERT INTO workspaces VALUES('w1','kept',NULL,'" + Now() + "')",
		"INSERT INTO migration_runs VALUES('r1','legacy','completed','digest',1,'legacy','" + Now() + "')"} {
		if _, err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// A database created by the v1 binary upgrades in place without losing rows and
// without replaying schema.sql. The released v1 ledger row keeps its checksum.
func TestUpgradeFromV1PreservesData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	createV1Database(t, path)

	// Opening read-write without create must refuse to silently upgrade.
	if _, err := Open(ctx, path, false); ErrorCode(err) != "invalid_input" {
		t.Fatalf("implicit upgrade: %v", err)
	}
	compatible, err := OpenReadCompatible(ctx, path)
	if err != nil {
		t.Fatalf("compatible read open: %v", err)
	}
	var compatibleName string
	if err = compatible.DB.QueryRowContext(ctx, "SELECT name FROM workspaces WHERE id='w1'").Scan(&compatibleName); err != nil || compatibleName != "kept" {
		t.Fatalf("compatible read: %q %v", compatibleName, err)
	}
	compatible.Close()
	var unchanged int
	legacyDB, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if err = legacyDB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	legacyDB.Close()
	if unchanged != 1 {
		t.Fatalf("compatible read upgraded schema to %d", unchanged)
	}
	up, err := Open(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	var name string
	if err = up.DB.QueryRowContext(ctx, "SELECT name FROM workspaces WHERE id='w1'").Scan(&name); err != nil || name != "kept" {
		t.Fatalf("v1 data lost: %q %v", name, err)
	}
	var version int
	if err = up.DB.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version %d: %v", version, err)
	}
	var checksum string
	if err = up.DB.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version=1").Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if checksum != Hash([]byte(schema)) {
		t.Fatal("released v1 migration was rewritten")
	}
	for _, table := range []string{"channels", "channel_routes", "principals", "identity_aliases", "conversations",
		"inbox_events", "messages", "message_revisions", "message_observations", "message_relations",
		"message_attachments", "message_recalls", "source_origins", "source_availability",
		"memory_publications", "request_contexts", "outbox", "delivery_attempts", "channel_leases",
		"coverage_windows", "channel_watermarks", "runtime_configs", "runtime_message_states", "runtime_batches",
		"runtime_batch_messages", "runtime_tasks", "runtime_task_messages", "runtime_attempts", "runtime_pending_actions",
		"runtime_action_attempts", "runtime_direct_sessions", "runtime_direct_turns"} {
		var n int
		if err = up.DB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("missing table %s: %v", table, err)
		}
	}
	// An existing migration_runs row gains defaults rather than a NULL purpose.
	var purpose, route string
	if err = up.DB.QueryRowContext(ctx, "SELECT purpose,route_id FROM migration_runs WHERE id='r1'").Scan(&purpose, &route); err != nil {
		t.Fatal(err)
	}
	if purpose != "migration" || route != "" {
		t.Fatalf("unexpected defaults %q %q", purpose, route)
	}
	report, err := up.Doctor(ctx)
	if err != nil || report["healthy"] != true {
		t.Fatalf("doctor after upgrade: %v %v", report, err)
	}
}

// An upgraded database and a freshly created one must agree on every object, so
// later migrations cannot diverge from schema.sql plus the migration chain.
func TestFreshAndUpgradedSchemasMatch(t *testing.T) {
	ctx := context.Background()
	objects := func(s *Store) []string {
		rows, err := s.DB.QueryContext(ctx, "SELECT type||' '||name||' '||coalesce(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var v string
			if err = rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	fresh, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	legacy := filepath.Join(t.TempDir(), "state.db")
	createV1Database(t, legacy)
	upgraded, err := Open(ctx, legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	a, b := objects(fresh), objects(upgraded)
	if len(a) != len(b) {
		t.Fatalf("object count differs: fresh %d upgraded %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("schema differs:\nfresh:    %s\nupgraded: %s", a[i], b[i])
		}
	}
}
