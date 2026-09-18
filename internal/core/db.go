package core

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	DB        *sql.DB
	Path      string
	lock      *os.File
	exclusive bool
	writeGate *storeWriteGate
	gateOnce  sync.Once
	// writeObserver is an offline-test seam. Production slow-write diagnostics
	// are emitted through slog without storing message content or database paths.
	writeObserver func(writeObservation)
}

type writeObservation struct {
	Command       string
	QueueDuration time.Duration
	BeginDuration time.Duration
	TxDuration    time.Duration
	ErrorCode     string
}

const slowWriteThreshold = 250 * time.Millisecond

type storeWriteGate struct {
	ready chan struct{}
	refs  int
}

var processWriteGates = struct {
	sync.Mutex
	byPath map[string]*storeWriteGate
}{byPath: map[string]*storeWriteGate{}}

func retainStoreWriteGate(path string) *storeWriteGate {
	processWriteGates.Lock()
	defer processWriteGates.Unlock()
	gate := processWriteGates.byPath[path]
	if gate == nil {
		gate = &storeWriteGate{ready: make(chan struct{}, 1)}
		gate.ready <- struct{}{}
		processWriteGates.byPath[path] = gate
	}
	gate.refs++
	return gate
}

func releaseStoreWriteGate(path string, gate *storeWriteGate) {
	if gate == nil {
		return
	}
	processWriteGates.Lock()
	defer processWriteGates.Unlock()
	if processWriteGates.byPath[path] != gate {
		return
	}
	gate.refs--
	if gate.refs == 0 {
		delete(processWriteGates.byPath, path)
	}
}

func (s *Store) acquireWrite(ctx context.Context) error {
	if s.writeGate == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return dbError(ctx.Err())
	case <-s.writeGate.ready:
		return nil
	}
}

func (s *Store) releaseWrite() {
	if s.writeGate != nil {
		s.writeGate.ready <- struct{}{}
	}
}

type Tx struct {
	Conn        *sql.Conn
	Request     Request
	OperationID string
}
type Queryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Open(ctx context.Context, path string, create bool) (*Store, error) {
	return openStore(ctx, path, create, false, false)
}

// OpenReadCompatible opens an older authoritative schema without upgrading it.
// Callers must only issue queries against tables and columns that existed in
// the database version they intend to support.
func OpenReadCompatible(ctx context.Context, path string) (*Store, error) {
	return openStore(ctx, path, false, false, true)
}
func openStore(ctx context.Context, path string, create, exclusive, allowOlder bool) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if errors.Is(statErr, os.ErrNotExist) && !create {
		return nil, Fail("not_found", "database not initialized; run memgov init")
	}
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	lock, err := lockFile(ctx, path, exclusive)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			unlockFile(lock)
		}
	}()
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_pragma", "foreign_keys(1)")
	waitMS := 5000
	if deadline, ok := ctx.Deadline(); ok {
		remaining := int(time.Until(deadline).Milliseconds())
		if remaining < 1 {
			remaining = 1
		}
		if remaining < waitMS {
			waitMS = remaining
		}
	}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", waitMS))
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "secure_delete(1)")
	if !create {
		q.Set("mode", "rw")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{DB: db, Path: path, lock: lock, exclusive: exclusive, writeGate: retainStoreWriteGate(path)}
	defer func() {
		if !ok {
			releaseStoreWriteGate(path, s.writeGate)
		}
	}()
	if err = s.acquireWrite(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initialize(ctx, create, allowOlder); err != nil {
		s.releaseWrite()
		db.Close()
		return nil, err
	}
	s.releaseWrite()
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	ok = true
	return s, nil
}
func (s *Store) Close() error {
	s.gateOnce.Do(func() { releaseStoreWriteGate(s.Path, s.writeGate) })
	return errors.Join(s.DB.Close(), unlockFile(s.lock))
}
func (s *Store) initialize(ctx context.Context, create, allowOlder bool) error {
	var name string
	err := s.DB.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE name='schema_migrations'").Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		var n int
		if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&n); err != nil {
			return err
		}
		if n > 0 || !create {
			return Fail("invalid_input", "not a memgov authoritative database; import legacy data into a new state.db")
		}
		conn, err := s.DB.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
		// Recheck under the write lock when two init processes start together.
		if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='schema_migrations'").Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err = conn.ExecContext(ctx, schema); err != nil {
				return fmt.Errorf("initialize schema: %w", err)
			}
			// Record only the baseline; upgradeSchema applies and records every
			// later migration so a fresh database and an upgraded one share one ledger.
			if _, err = conn.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(?,?,?)", 1, Hash([]byte(schema)), Now()); err != nil {
				return err
			}
			if _, err = conn.ExecContext(ctx, "INSERT INTO settings VALUES('role','authoritative')"); err != nil {
				return err
			}
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	var version int
	if err = s.DB.QueryRowContext(ctx, "SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1").Scan(&version); err != nil {
		return err
	}
	if allowOlder && version < databaseMigrations[len(databaseMigrations)-1].Version {
		if err = s.validateSchemaLedger(ctx, version, databaseMigrations); err != nil {
			return err
		}
	} else {
		if err = s.upgradeSchema(ctx, version, create, databaseMigrations); err != nil {
			return err
		}
	}
	var mode string
	if err = s.DB.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return Fail("internal", "WAL mode unavailable")
	}
	return nil
}

// Mutate owns the transaction boundary. Domain state, audit and cached response
// become visible together; no filesystem side effects are allowed in fn.
func (s *Store) Mutate(ctx context.Context, req Request, fn func(*Tx) (any, error)) (result Result, retErr error) {
	queuedAt := time.Now()
	acquiredAt := queuedAt
	databasePhaseStarted := false
	transactionAt := acquiredAt
	transactionStarted := false
	defer func() {
		observation := writeObservation{Command: req.Command, QueueDuration: acquiredAt.Sub(queuedAt)}
		if retErr != nil {
			observation.ErrorCode = ErrorCode(retErr)
		}
		if databasePhaseStarted {
			if transactionStarted {
				observation.BeginDuration = transactionAt.Sub(acquiredAt)
				observation.TxDuration = time.Since(transactionAt)
			} else {
				observation.BeginDuration = time.Since(acquiredAt)
			}
		}
		if s.writeObserver != nil {
			s.writeObserver(observation)
		}
		if observation.QueueDuration >= slowWriteThreshold || observation.BeginDuration >= slowWriteThreshold || observation.TxDuration >= slowWriteThreshold {
			outcome := "completed"
			if retErr != nil {
				outcome = "failed"
			}
			slog.Warn("memgov sqlite write delayed",
				"command", observation.Command,
				"queue_ms", observation.QueueDuration.Milliseconds(),
				"begin_ms", observation.BeginDuration.Milliseconds(),
				"transaction_ms", observation.TxDuration.Milliseconds(),
				"outcome", outcome,
				"error_code", observation.ErrorCode)
		}
	}()
	if err := s.acquireWrite(ctx); err != nil {
		acquiredAt = time.Now()
		return Result{}, err
	}
	acquiredAt = time.Now()
	databasePhaseStarted = true
	defer s.releaseWrite()
	if req.ID == "" {
		req.ID = NewID()
	}
	if req.Actor == "" {
		req.Actor = "cli"
	}
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return Result{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Result{}, dbError(err)
	}
	transactionAt = time.Now()
	transactionStarted = true
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	hash := Digest(req.Input)
	if req.Key != "" {
		var priorHash, raw string
		err = conn.QueryRowContext(ctx, "SELECT request_hash,result FROM idempotency WHERE scope=? AND command=? AND key=?", req.Scope, req.Command, req.Key).Scan(&priorHash, &raw)
		if err == nil {
			if priorHash != hash {
				return Result{}, Fail("conflict", "idempotency key already used for a different input")
			}
			raw, err = redactCachedRetentionResult(ctx, conn, raw)
			if err != nil {
				return Result{}, err
			}
			return Result{json.RawMessage(raw), true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Result{}, err
		}
	}
	tx := &Tx{Conn: conn, Request: req}
	out, err := fn(tx)
	if err != nil {
		return Result{}, dbError(err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return Result{}, err
	}
	if _, err = conn.ExecContext(ctx, "INSERT INTO requests VALUES(?,?,?,?,?,?,?)", req.ID, req.Command, req.Actor, 1, "", tx.OperationID, Now()); err != nil {
		return Result{}, err
	}
	if req.Key != "" {
		if _, err = conn.ExecContext(ctx, "INSERT INTO idempotency VALUES(?,?,?,?,?,?)", req.Scope, req.Command, req.Key, hash, string(raw), Now()); err != nil {
			return Result{}, err
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Result{}, dbError(err)
	}
	return Result{raw, false}, nil
}
func (tx *Tx) Audit(ctx context.Context, kind, reason string, changes []Change) (Operation, error) {
	op := Operation{NewID(), tx.Request.ID, kind, tx.Request.Actor, reason, changes, Now()}
	_, err := tx.Conn.ExecContext(ctx, "INSERT INTO operations VALUES(?,?,?,?,?,?,?)", op.ID, op.RequestID, op.Kind, op.Actor, op.Reason, JSON(op.Changes), op.CreatedAt)
	tx.OperationID = op.ID
	return op, err
}
func dbError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Fail("unavailable", "operation canceled or timed out")
	}
	v := err.Error()
	if strings.Contains(v, "SQLITE_BUSY") || strings.Contains(v, "database is locked") {
		return Fail("unavailable", "database busy; retry the same request")
	}
	return err
}

func (s *Store) Workspace(ctx context.Context, value string) (Workspace, error) {
	if value == "" {
		value = "global"
	}
	var w Workspace
	err := s.DB.QueryRowContext(ctx, "SELECT id,name,coalesce(path,'') FROM workspaces WHERE id=? OR name=?", value, value).Scan(&w.ID, &w.Name, &w.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return w, Fail("not_found", "workspace %q not found", value)
	}
	return w, err
}
func (s *Store) Workspaces(ctx context.Context) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id,name,coalesce(path,'') FROM workspaces ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		var w Workspace
		if err = rows.Scan(&w.ID, &w.Name, &w.Path); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (tx *Tx) AddWorkspace(ctx context.Context, name, path string) (Workspace, error) {
	if strings.TrimSpace(name) == "" {
		return Workspace{}, Fail("invalid_input", "workspace name is required")
	}
	if path != "" {
		var err error
		path, err = filepath.Abs(path)
		if err != nil {
			return Workspace{}, err
		}
	}
	w := Workspace{NewID(), name, path}
	var p any
	if path != "" {
		p = path
	}
	_, err := tx.Conn.ExecContext(ctx, "INSERT INTO workspaces VALUES(?,?,?,?)", w.ID, w.Name, p, Now())
	if err != nil {
		return w, Fail("conflict", "workspace name or path already registered: %v", err)
	}
	_, err = tx.Audit(ctx, "workspace.create", name, objectChange("workspace", w.ID))
	return w, err
}
func (s *Store) Doctor(ctx context.Context) (map[string]any, error) {
	var check string
	if err := s.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fkOK := !rows.Next()
	counts := map[string]int{}
	for _, table := range []string{"workspaces", "sources", "fragments", "memories", "revisions", "candidates", "jobs", "operations", "tombstones"} {
		var n int
		if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			return nil, err
		}
		counts[table] = n
	}
	issues, err := checkDerivedAndEvidence(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	return map[string]any{"healthy": check == "ok" && fkOK && len(issues) == 0, "integrity": check, "foreign_keys": fkOK, "schema_version": SchemaVersion, "db_path": s.Path, "counts": counts, "issues": issues}, nil
}
