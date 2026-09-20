package core

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed schema_v2.sql
var schemaV2 string

//go:embed schema_v3.sql
var schemaV3 string

//go:embed schema_v4.sql
var schemaV4 string

//go:embed schema_v5.sql
var schemaV5 string

//go:embed schema_v6.sql
var schemaV6 string

//go:embed schema_v7.sql
var schemaV7 string

//go:embed schema_v8.sql
var schemaV8 string

//go:embed schema_v9.sql
var schemaV9 string

//go:embed schema_v10.sql
var schemaV10 string

//go:embed schema_v11.sql
var schemaV11 string

//go:embed schema_v12.sql
var schemaV12 string

//go:embed schema_v13.sql
var schemaV13 string

//go:embed schema_v14.sql
var schemaV14 string

//go:embed schema_v15.sql
var schemaV15 string

//go:embed schema_v16.sql
var schemaV16 string

//go:embed schema_v17.sql
var schemaV17 string

//go:embed schema_v18.sql
var schemaV18 string

//go:embed schema_v19.sql
var schemaV19 string

//go:embed schema_v20.sql
var schemaV20 string

//go:embed schema_v21.sql
var schemaV21 string

//go:embed schema_v22.sql
var schemaV22 string

//go:embed schema_v23.sql
var schemaV23 string

//go:embed schema_v24.sql
var schemaV24 string

//go:embed schema_v25.sql
var schemaV25 string

//go:embed schema_v26.sql
var schemaV26 string

func init() {
	databaseMigrations = append(databaseMigrations, databaseMigration{Version: 23, SQL: schemaV23})
	databaseMigrations = append(databaseMigrations, databaseMigration{Version: 24, SQL: schemaV24})
	databaseMigrations = append(databaseMigrations, databaseMigration{Version: 25, SQL: schemaV25})
	databaseMigrations = append(databaseMigrations, databaseMigration{Version: 26, SQL: schemaV26})
}

type databaseMigration struct {
	Version int
	SQL     string
}

// Append migrations; never modify a released migration or replay schema.sql.
var databaseMigrations = []databaseMigration{{Version: 1, SQL: schema}, {Version: 2, SQL: schemaV2}, {Version: 3, SQL: schemaV3}, {Version: 4, SQL: schemaV4}, {Version: 5, SQL: schemaV5}, {Version: 6, SQL: schemaV6}, {Version: 7, SQL: schemaV7}, {Version: 8, SQL: schemaV8}, {Version: 9, SQL: schemaV9}, {Version: 10, SQL: schemaV10}, {Version: 11, SQL: schemaV11}, {Version: 12, SQL: schemaV12}, {Version: 13, SQL: schemaV13}, {Version: 14, SQL: schemaV14}, {Version: 15, SQL: schemaV15}, {Version: 16, SQL: schemaV16}, {Version: 17, SQL: schemaV17}, {Version: 18, SQL: schemaV18}, {Version: 19, SQL: schemaV19}, {Version: 20, SQL: schemaV20}, {Version: 21, SQL: schemaV21}, {Version: 22, SQL: schemaV22}}

func (s *Store) validateSchemaLedger(ctx context.Context, current int, migrations []databaseMigration) error {
	target := migrations[len(migrations)-1].Version
	if current < 1 || current > target {
		return Fail("invalid_input", "database schema %d is not supported", current)
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT version,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var version int
		var checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			return err
		}
		seen++
		if version != seen || version > current || checksum != Hash([]byte(migrations[version-1].SQL)) {
			return Fail("invalid_input", "schema migration ledger is invalid at version %d", version)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if seen != current {
		return Fail("invalid_input", "schema migration ledger has a gap at %d", seen+1)
	}
	return nil
}

func (s *Store) upgradeSchema(ctx context.Context, current int, allow bool, migrations []databaseMigration) error {
	target := migrations[len(migrations)-1].Version
	if current > target {
		return Fail("invalid_input", "database schema %d is newer than supported schema %d", current, target)
	}
	if current < target && !allow {
		return Fail("invalid_input", "schema upgrade required; run memgov init with this binary")
	}
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return dbError(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	// Verify the complete ledger, including historical migrations, under the lock.
	rows, err := conn.QueryContext(ctx, "SELECT version,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return err
	}
	applied := map[int]string{}
	for rows.Next() {
		var v int
		var hash string
		if err = rows.Scan(&v, &hash); err != nil {
			rows.Close()
			return err
		}
		applied[v] = hash
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(applied) > len(migrations) {
		return Fail("invalid_input", "unrecognized schema migration ledger")
	}
	for i, m := range migrations {
		if m.Version != i+1 {
			return fmt.Errorf("non-contiguous compiled migration chain")
		}
		if hash, ok := applied[m.Version]; ok {
			if hash != Hash([]byte(m.SQL)) {
				return Fail("invalid_input", "schema migration %d checksum mismatch", m.Version)
			}
			continue
		}
		if m.Version <= current {
			return Fail("invalid_input", "schema migration ledger has a gap at %d", m.Version)
		}
		if !allow {
			return Fail("invalid_input", "schema upgrade required; run memgov init")
		}
		if _, err = conn.ExecContext(ctx, m.SQL); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(?,?,?)", m.Version, Hash([]byte(m.SQL)), Now()); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}
