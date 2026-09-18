package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type BackupInfo struct {
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	Bytes         int64  `json:"bytes"`
	SchemaVersion int    `json:"schema_version"`
	CreatedAt     string `json:"created_at"`
	Integrity     string `json:"integrity"`
}

func fileHash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
func VerifyBackup(ctx context.Context, path string) (BackupInfo, error) {
	return verifyBackup(ctx, path, false)
}

func verifyBackup(ctx context.Context, path string, allowOlder bool) (BackupInfo, error) {
	var b BackupInfo
	abs, err := filepath.Abs(path)
	if err != nil {
		return b, err
	}
	b.Path = abs
	if info, e := os.Stat(abs + "-wal"); e == nil && info.Size() > 0 {
		return b, Fail("invalid_input", "backup has a live WAL sidecar; create a consistent backup first")
	}
	b.SHA256, b.Bytes, err = fileHash(abs)
	if err != nil {
		return b, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return b, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&b.Integrity); err != nil {
		return b, Fail("invalid_input", "invalid backup database: %v", err)
	}
	if b.Integrity != "ok" {
		return b, Fail("invalid_input", "backup integrity check failed")
	}
	var checksum, role string
	if err = db.QueryRowContext(ctx, "SELECT version,checksum,applied_at FROM schema_migrations ORDER BY version DESC LIMIT 1").Scan(&b.SchemaVersion, &checksum, &b.CreatedAt); err != nil {
		return b, Fail("invalid_input", "backup schema metadata is missing")
	}
	latest := databaseMigrations[len(databaseMigrations)-1]
	if allowOlder {
		if err := (&Store{DB: db}).validateSchemaLedger(ctx, b.SchemaVersion, databaseMigrations); err != nil {
			return b, err
		}
	} else if b.SchemaVersion != SchemaVersion || checksum != Hash([]byte(latest.SQL)) {
		return b, Fail("invalid_input", "backup schema is not supported by this binary")
	}
	if err = db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='role'").Scan(&role); err != nil || role != "authoritative" {
		return b, Fail("invalid_input", "not an authoritative memgov backup")
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return b, err
	}
	bad := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return b, err
	}
	if bad {
		return b, Fail("invalid_input", "backup foreign key check failed")
	}
	if info, e := os.Stat(abs); e == nil {
		b.CreatedAt = info.ModTime().UTC().Format("2006-01-02T15:04:05.999999999Z")
	}
	return b, nil
}
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func (s *Store) CreateBackup(ctx context.Context, path string) (BackupInfo, error) {
	return s.createBackup(ctx, path, false)
}

func (s *Store) createBackup(ctx context.Context, path string, allowOlder bool) (BackupInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return BackupInfo{}, err
	}
	if abs == s.Path {
		return BackupInfo{}, Fail("invalid_input", "backup cannot replace the active database")
	}
	if _, err = os.Stat(abs); err == nil {
		return BackupInfo{}, Fail("conflict", "backup destination already exists")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return BackupInfo{}, err
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return BackupInfo{}, err
	}
	temp := abs + ".tmp-" + NewID()
	defer os.Remove(temp)
	// VACUUM INTO creates a transactionally consistent independent database.
	if _, err = s.DB.ExecContext(ctx, "VACUUM INTO ?", temp); err != nil {
		return BackupInfo{}, dbError(err)
	}
	if err = os.Chmod(temp, 0600); err != nil {
		return BackupInfo{}, err
	}
	if err = syncFile(temp); err != nil {
		return BackupInfo{}, err
	}
	b, err := verifyBackup(ctx, temp, allowOlder)
	if err != nil {
		return b, err
	}
	// Link publishes without replacing an existing file, including racing writers.
	if err = os.Link(temp, abs); err != nil {
		return b, Fail("conflict", "backup destination already exists or cannot be published: %v", err)
	}
	if err = syncDir(filepath.Dir(abs)); err != nil {
		return b, err
	}
	b.Path = abs
	return b, nil
}
func ListBackups(ctx context.Context, dir string) ([]BackupInfo, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []BackupInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []BackupInfo{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".db") {
			b, err := VerifyBackup(ctx, filepath.Join(dir, e.Name()))
			if err != nil {
				return out, err
			}
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}
func Tombstones(ctx context.Context, q Queryer) ([]Tombstone, error) {
	rows, err := q.QueryContext(ctx, "SELECT kind,fingerprint,operation_id,created_at FROM tombstones ORDER BY kind,fingerprint")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tombstone{}
	for rows.Next() {
		var t Tombstone
		if err = rows.Scan(&t.Kind, &t.Fingerprint, &t.OperationID, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (tx *Tx) EnforceTombstones(ctx context.Context, existing []Tombstone) (PurgeManifest, error) {
	empty := PurgeManifest{}
	for _, t := range existing {
		if _, err := tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO tombstones VALUES(?,?,?,?)", t.Kind, t.Fingerprint, t.OperationID, t.CreatedAt); err != nil {
			return empty, err
		}
	}
	all, err := Tombstones(ctx, tx.Conn)
	if err != nil {
		return empty, err
	}
	set := map[string]bool{}
	for _, t := range all {
		set[t.Kind+":"+t.Fingerprint] = true
	}
	memorySeeds, sourceSeeds := []string{}, []string{}
	rows, err := tx.Conn.QueryContext(ctx, "SELECT id,content FROM memories")
	if err != nil {
		return empty, err
	}
	for rows.Next() {
		var id, body string
		if err = rows.Scan(&id, &body); err != nil {
			rows.Close()
			return empty, err
		}
		if set["memory_id:"+id] || set["content:"+Hash([]byte(strings.TrimSpace(body)))] {
			memorySeeds = append(memorySeeds, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return empty, err
	}
	rows, err = tx.Conn.QueryContext(ctx, "SELECT s.id,s.digest,s.lineage,coalesce(l.uri,''),s.content FROM sources s LEFT JOIN source_locations l ON l.source_id=s.id WHERE redacted=0")
	if err != nil {
		return empty, err
	}
	seen := map[string]bool{}
	for rows.Next() {
		var id, digest, lineage, uri, body string
		if err = rows.Scan(&id, &digest, &lineage, &uri, &body); err != nil {
			rows.Close()
			return empty, err
		}
		if set["source_id:"+id] || set["source_digest:"+digest] || set["source_lineage:"+lineage] || set["source_uri:"+Hash([]byte(uri))] || set["content:"+Hash([]byte(strings.TrimSpace(body)))] {
			seen[id] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return empty, err
	}
	sourceSeeds = keys(seen)
	p, _, err := purgeClosure(ctx, tx.Conn, memorySeeds, sourceSeeds)
	if err != nil {
		return p, err
	}
	if len(all) > 0 {
		op, e := tx.Audit(ctx, "backup.enforce_tombstones", "preserve current removal rules", nil)
		if e != nil {
			return p, e
		}
		err = tx.redact(ctx, p, op.ID)
	}
	return p, err
}

// Restore replaces the database only after validation and tombstone enforcement
// in an isolated staging database. The current lock spans the atomic rename.
func RestoreBackup(ctx context.Context, path, backup, digest string, req Request) (any, error) {
	b, err := VerifyBackup(ctx, backup)
	if err != nil {
		return nil, err
	}
	if digest == "" || b.SHA256 != digest {
		return nil, Fail("conflict", "expected-digest must match the verified backup")
	}
	current, err := OpenMaintenance(ctx, path)
	if err != nil {
		return nil, err
	}
	defer current.Close()
	if b.Path == current.Path {
		return nil, Fail("invalid_input", "cannot restore from the active database")
	}
	if req.Key != "" {
		var hash, raw string
		e := current.DB.QueryRowContext(ctx, "SELECT request_hash,result FROM idempotency WHERE scope=? AND command=? AND key=?", req.Scope, req.Command, req.Key).Scan(&hash, &raw)
		if e == nil {
			if hash != Digest(req.Input) {
				return nil, Fail("conflict", "idempotency key input changed")
			}
			return json.RawMessage(raw), nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
	}
	tombs, err := Tombstones(ctx, current.DB)
	if err != nil {
		return nil, err
	}
	history, err := readPurgeHistory(ctx, current.DB)
	if err != nil {
		return nil, err
	}
	temp := current.Path + ".restore-" + NewID()
	defer func() { os.Remove(temp); os.Remove(temp + "-wal"); os.Remove(temp + "-shm"); os.Remove(temp + ".lock") }()
	src, err := os.Open(b.Path)
	if err != nil {
		return nil, err
	}
	dst, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		src.Close()
		return nil, err
	}
	_, copyErr := io.Copy(dst, src)
	err = errors.Join(copyErr, src.Close(), dst.Sync(), dst.Close())
	if err != nil {
		return nil, err
	}
	hash, _, err := fileHash(temp)
	if err != nil {
		return nil, err
	}
	if hash != digest {
		return nil, Fail("conflict", "backup changed while copying")
	}
	stage, err := OpenMaintenance(ctx, temp)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	// Restored command caches and leases are not safe to replay into a new run.
	if _, err = stage.DB.ExecContext(ctx, "DELETE FROM idempotency; UPDATE jobs SET status='pending',lease_token='',lease_until='',worker='' WHERE status='leased'"); err != nil {
		return nil, err
	}
	result, err := stage.Mutate(ctx, req, func(tx *Tx) (any, error) {
		if e := tx.retainPurgeHistory(ctx, history); e != nil {
			return nil, e
		}
		p, e := tx.EnforceTombstones(ctx, tombs)
		if e != nil {
			return nil, e
		}
		op, e := tx.Audit(ctx, "backup.restore", digest, nil)
		return map[string]any{"restored_from": b.Path, "backup_sha256": digest, "preserved_tombstones": len(tombs), "redacted_memories": len(p.Memories), "operation": op}, e
	})
	if err != nil {
		return nil, err
	}
	if err = checkpoint(ctx, stage.DB); err != nil {
		return nil, err
	}
	if _, err = stage.DB.ExecContext(ctx, "VACUUM"); err != nil {
		return nil, err
	}
	if err = checkpoint(ctx, stage.DB); err != nil {
		return nil, err
	}
	if err = stage.Close(); err != nil {
		return nil, err
	}
	if err = syncFile(temp); err != nil {
		return nil, err
	}
	if _, err = VerifyBackup(ctx, temp); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, dbError(err)
	}
	if err = checkpoint(ctx, current.DB); err != nil {
		return nil, err
	}
	if err = current.DB.Close(); err != nil {
		return nil, err
	}
	// Checkpoint made the old main file self-contained. Empty old sidecars must
	// never be replayed against the replacement database.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = os.Remove(current.Path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err = os.Rename(temp, current.Path); err != nil {
		return nil, err
	}
	if err = syncDir(filepath.Dir(current.Path)); err != nil {
		return nil, err
	}
	return result.Data, nil
}
