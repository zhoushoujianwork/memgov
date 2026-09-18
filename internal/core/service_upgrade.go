package core

import (
	"context"
	"fmt"
	"path/filepath"
)

// PrepareServiceDatabase upgrades an existing database for an enrolled managed
// service. Ordinary commands still require explicit init. The service lock must
// be held by the caller; the exclusive database lock also excludes CLI writers.
// A verified, durable snapshot of the old schema is published before migration.
func PrepareServiceDatabase(ctx context.Context, path string) (*BackupInfo, error) {
	s, err := OpenReadCompatible(ctx, path)
	if err != nil {
		return nil, err
	}
	var version int
	err = s.DB.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version)
	closeErr := s.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if version == SchemaVersion {
		return nil, nil
	}
	s, err = openStore(ctx, path, false, true, true)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	// Another upgrader may have finished while we waited for exclusive access.
	if err = s.DB.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		return nil, err
	}
	if version == SchemaVersion {
		return nil, nil
	}
	destination := filepath.Join(filepath.Dir(s.Path), "backups", "service-upgrades", fmt.Sprintf("v%d-to-v%d-%s.db", version, SchemaVersion, NewID()))
	backup, err := s.createBackup(ctx, destination, true)
	if err != nil {
		return nil, fmt.Errorf("service database upgrade: backup failed; schema unchanged: %w", err)
	}
	if err = s.upgradeSchema(ctx, version, true, databaseMigrations); err != nil {
		return &backup, fmt.Errorf("service database upgrade failed; backup at %s: %w", backup.Path, err)
	}
	return &backup, nil
}
