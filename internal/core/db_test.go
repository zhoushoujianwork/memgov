package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestDatabaseAtomicAuditAndIdempotency(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	req := Request{Command: "workspace.add", Scope: "global", Key: "one", Input: map[string]any{"name": "project"}}
	result, err := s.Mutate(ctx, req, func(tx *Tx) (any, error) { return tx.AddWorkspace(ctx, "project", "") })
	if err != nil {
		t.Fatal(err)
	}
	var w Workspace
	if err = json.Unmarshal(result.Data, &w); err != nil {
		t.Fatal(err)
	}
	cached, err := s.Mutate(ctx, req, func(*Tx) (any, error) { t.Fatal("cached request executed twice"); return nil, nil })
	if err != nil || !cached.Cached {
		t.Fatalf("cache: %v %+v", err, cached)
	}
	req.Input = map[string]any{"name": "other"}
	if _, err = s.Mutate(ctx, req, func(*Tx) (any, error) { return nil, nil }); ErrorCode(err) != "conflict" {
		t.Fatalf("mismatched request: %v", err)
	}
	_, err = s.Mutate(ctx, Request{Command: "fail"}, func(tx *Tx) (any, error) {
		if _, err := tx.AddWorkspace(ctx, "rolled-back", ""); err != nil {
			return nil, err
		}
		return nil, Fail("internal", "injected failure")
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	if _, err = s.Workspace(ctx, "rolled-back"); ErrorCode(err) != "not_found" {
		t.Fatalf("partial write: %v", err)
	}
	var n int
	if err = s.DB.QueryRow("SELECT count(*) FROM operations").Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit: %d %v", n, err)
	}
}
func TestOpenDoesNotCreateMissingDatabase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing", "state.db")
	if _, err := Open(context.Background(), p, false); ErrorCode(err) != "not_found" {
		t.Fatal(err)
	}
}

func TestStoresSerializeWritesToTheSameDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := Open(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := Open(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	if first.writeGate != second.writeGate {
		t.Fatal("stores for the same database did not share a write gate")
	}
	for _, store := range []*Store{first, second} {
		store.DB.SetMaxOpenConns(1)
		if _, err = store.DB.ExecContext(ctx, "PRAGMA busy_timeout=1"); err != nil {
			t.Fatal(err)
		}
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	firstErr := make(chan error, 1)
	go func() {
		_, mutateErr := first.Mutate(ctx, Request{Scope: "global", Command: "test.concurrent.first"}, func(tx *Tx) (any, error) {
			close(entered)
			<-release
			return tx.AddWorkspace(ctx, "first", "")
		})
		firstErr <- mutateErr
	}()
	<-entered
	secondErr := make(chan error, 1)
	observed := make(chan writeObservation, 1)
	secondStarted := make(chan struct{})
	second.writeObserver = func(value writeObservation) { observed <- value }
	go func() {
		close(secondStarted)
		_, mutateErr := second.Mutate(ctx, Request{Scope: "global", Command: "test.concurrent.second"}, func(tx *Tx) (any, error) {
			return tx.AddWorkspace(ctx, "second", "")
		})
		secondErr <- mutateErr
	}()
	<-secondStarted
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err = <-firstErr; err != nil {
		t.Fatal(err)
	}
	if err = <-secondErr; err != nil {
		t.Fatalf("second store saw transient SQLite contention: %v", err)
	}
	observation := <-observed
	if observation.Command != "test.concurrent.second" || observation.QueueDuration < 15*time.Millisecond || observation.TxDuration <= 0 || observation.ErrorCode != "" {
		t.Fatalf("write timing was not observed accurately: %+v", observation)
	}
}

func TestSchemaVersionAndForeignKeys(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec("INSERT INTO sources VALUES('bad','missing','note','body','hash','','now','',0)"); err == nil {
		t.Fatal("foreign keys disabled")
	}
	if _, err := s.DB.Exec("UPDATE schema_migrations SET version=999 WHERE version=?", SchemaVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, s.Path, false); ErrorCode(err) != "invalid_input" {
		t.Fatal(err)
	}
}

func TestSchemaUpgradeIsVersionedAndAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Probe one migration beyond the released chain so the released migrations keep
	// their real recorded checksums while the upgrade path itself is exercised.
	next := SchemaVersion + 1
	chain := func(sql string) []databaseMigration {
		return append(append([]databaseMigration{}, databaseMigrations...), databaseMigration{next, sql})
	}
	fail := chain("CREATE TABLE migration_probe(id TEXT); SELECT missing_column FROM missing_table;")
	if err := s.upgradeSchema(ctx, SchemaVersion, true, fail); err == nil {
		t.Fatal("expected injected migration failure")
	}
	var n int
	s.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='migration_probe'").Scan(&n)
	if n != 0 {
		t.Fatal("partial schema upgrade")
	}
	migrations := chain("CREATE TABLE migration_probe(id TEXT);")
	if err := s.upgradeSchema(ctx, SchemaVersion, false, migrations); ErrorCode(err) != "invalid_input" {
		t.Fatal(err)
	}
	if err := s.upgradeSchema(ctx, SchemaVersion, true, migrations); err != nil {
		t.Fatal(err)
	}
	if err := s.upgradeSchema(ctx, next, true, migrations); err != nil {
		t.Fatal("upgrade replay", err)
	}
	s.DB.Exec("UPDATE schema_migrations SET checksum='changed' WHERE version=1")
	if err := s.upgradeSchema(ctx, next, true, migrations); ErrorCode(err) != "invalid_input" {
		t.Fatal("historical checksum not checked", err)
	}
}
func TestDoctorFindsFTSDriftAndRebuildRepairsIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fixtureSource(t, s)
	s.DB.Exec("DELETE FROM search_fts")
	report, err := s.Doctor(ctx)
	if err != nil || report["healthy"] != false {
		t.Fatal(report, err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.Reindex(ctx) })
	if err != nil {
		t.Fatal(err)
	}
	report, err = s.Doctor(ctx)
	if err != nil || report["healthy"] != true {
		t.Fatal(report, err)
	}
}
func TestAuditFailureRollsBackSourceAndIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, err := s.DB.Exec("CREATE TRIGGER reject_audit BEFORE INSERT ON operations BEGIN SELECT RAISE(ABORT,'injected audit failure'); END;")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		return tx.Ingest(ctx, SourceInput{URI: "test://audit", Content: "must roll back"})
	})
	if err == nil {
		t.Fatal("audit failure swallowed")
	}
	var n int
	s.DB.QueryRow("SELECT count(*) FROM sources").Scan(&n)
	if n != 0 {
		t.Fatal("source committed without audit")
	}
}
