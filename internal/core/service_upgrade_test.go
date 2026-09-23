package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func serviceUpgradeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dropSchema23(t, s)
	for _, statement := range []string{
		"INSERT INTO settings VALUES('service-upgrade-marker','keep me')",
		"DROP TABLE runtime_message_actions",
		"DELETE FROM schema_migrations WHERE version=22",
	} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func assertServiceSchema(t *testing.T, path string, want int) {
	t.Helper()
	s, err := OpenReadCompatible(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	var marker string
	if err := s.DB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != want {
		t.Fatalf("schema %d, want %d: %v", version, want, err)
	}
	if err := s.DB.QueryRow("SELECT value FROM settings WHERE key='service-upgrade-marker'").Scan(&marker); err != nil || marker != "keep me" {
		t.Fatalf("lost original data: %q %v", marker, err)
	}
}

func TestPrepareServiceDatabase(t *testing.T) {
	ctx := context.Background()
	path := serviceUpgradeFixture(t)
	if _, err := Open(ctx, path, false); err == nil {
		t.Fatal("ordinary open silently migrated")
	}
	b, err := PrepareServiceDatabase(ctx, path)
	if err != nil || b == nil {
		t.Fatalf("upgrade: %+v %v", b, err)
	}
	if b.SchemaVersion != 21 || b.Integrity != "ok" || b.SHA256 == "" {
		t.Fatalf("invalid backup: %+v", b)
	}
	verified, err := verifyBackup(ctx, b.Path, true)
	if err != nil || verified.SHA256 != b.SHA256 {
		t.Fatalf("published backup invalid: %+v %v", verified, err)
	}
	if _, err := VerifyBackup(ctx, b.Path); err != nil {
		t.Fatal("old archive verification", err)
	}
	if info, err := os.Stat(b.Path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions: %v %v", info, err)
	}
	assertServiceSchema(t, b.Path, 21)
	assertServiceSchema(t, path, SchemaVersion)
	if b, err := PrepareServiceDatabase(ctx, path); err != nil || b != nil {
		t.Fatalf("repeated upgrade: %+v %v", b, err)
	}
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(path), "backups", "service-upgrades", "*.db"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("unnecessary backups: %v %v", entries, err)
	}
}

func TestPrepareServiceDatabaseBackupFailure(t *testing.T) {
	path := serviceUpgradeFixture(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "backups"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareServiceDatabase(context.Background(), path); err == nil || !strings.Contains(err.Error(), "archive failed") {
		t.Fatalf("backup failure: %v", err)
	}
	assertServiceSchema(t, path, 21)
}

func TestPrepareServiceDatabaseExcludesActiveClients(t *testing.T) {
	path := serviceUpgradeFixture(t)
	s, err := OpenReadCompatible(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := PrepareServiceDatabase(ctx, path); err == nil {
		t.Fatal("upgraded with active database client")
	}
	assertServiceSchema(t, path, 21)
}

func TestPrepareServiceDatabaseMigrationFailureKeepsBackup(t *testing.T) {
	path := serviceUpgradeFixture(t)
	s, err := OpenReadCompatible(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	// Force the bundled migration to fail after backup publication.
	if _, err := s.DB.Exec("CREATE TABLE runtime_message_actions(blocked INTEGER)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	b, err := PrepareServiceDatabase(context.Background(), path)
	if err == nil || b == nil || !strings.Contains(err.Error(), b.Path) {
		t.Fatalf("migration failure lost backup diagnostic: %+v %v", b, err)
	}
	assertServiceSchema(t, path, 21)
	assertServiceSchema(t, b.Path, 21)
}

func TestPrepareServiceDatabaseRejectsUnsupported(t *testing.T) {
	for _, statement := range []string{
		"UPDATE schema_migrations SET checksum='tampered' WHERE version=1",
		"INSERT INTO schema_migrations VALUES(999,'future','today')",
	} {
		t.Run(statement, func(t *testing.T) {
			path := serviceUpgradeFixture(t)
			s, err := OpenReadCompatible(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.Exec(statement); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if _, err := PrepareServiceDatabase(context.Background(), path); err == nil {
				t.Fatal("accepted unsupported database")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), "backups")); !os.IsNotExist(err) {
				t.Fatalf("backup attempted for unsupported schema: %v", err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "state.db")
	if _, err := PrepareServiceDatabase(context.Background(), path); err == nil {
		t.Fatal("created missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created missing database: %v", err)
	}
}
