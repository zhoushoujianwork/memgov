package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func ReadMemory(ctx context.Context, q Queryer, id, scope string, version int) (Memory, error) {
	var raw string
	var err error
	if version > 0 {
		err = q.QueryRowContext(ctx, "SELECT r.document FROM revisions r JOIN memories m ON m.id=r.memory_id WHERE r.memory_id=? AND r.version=? AND m.workspace_id IN (?,'global')", id, version, scopeID(scope)).Scan(&raw)
	} else {
		err = q.QueryRowContext(ctx, "SELECT document FROM memories WHERE id=? AND workspace_id IN (?,'global')", id, scopeID(scope)).Scan(&raw)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Memory{}, Fail("not_found", "memory or revision not found in this workspace: %s", id)
	}
	if err != nil {
		return Memory{}, err
	}
	var m Memory
	err = json.Unmarshal([]byte(raw), &m)
	if err == nil {
		err = redactReadMemoryQuotes(ctx, q, &m)
	}
	return m, err
}
func (tx *Tx) SaveMemories(ctx context.Context, kind, reason string, items []Memory, expected map[string]int) ([]Memory, Operation, error) {
	if reason == "" {
		return nil, Operation{}, Fail("invalid_input", "reason is required")
	}
	changes := []Change{}
	for i := range items {
		m := &items[i]
		if m.ID == "" {
			m.ID = NewID()
		}
		before := expected[m.ID]
		if m.Status == "" || m.Status == "active" {
			var conflicts int
			if err := tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM relations WHERE kind='conflicts_with' AND (src_id=? OR dst_id=?)", m.ID, m.ID).Scan(&conflicts); err != nil {
				return nil, Operation{}, err
			}
			if conflicts > 0 {
				return nil, Operation{}, Fail("conflict", "resolve open conflict links before reactivating this memory")
			}
		}

		if m.WorkspaceID == "global" {
			m.WorkspaceID = ""
		}
		if scopeID(m.WorkspaceID) != scopeID(tx.Request.Scope) {
			return nil, Operation{}, Fail("denied", "write requires the memory's own workspace")
		}
		if err := ValidateMemory(*m); err != nil {
			return nil, Operation{}, err
		}
		if err := checkEvidence(ctx, tx.Conn, *m); err != nil {
			return nil, Operation{}, err
		}
		m.Version = before + 1
		if m.Status == "" {
			m.Status = "active"
		}
		changes = append(changes, Change{MemoryID: m.ID, Before: before, After: m.Version})
	}
	op, err := tx.Audit(ctx, kind, reason, changes)
	if err != nil {
		return nil, op, err
	}
	for _, m := range items {
		before := expected[m.ID]
		doc := JSON(m)
		if before == 0 {
			if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO memories VALUES(?,?,?,?,?,?,?,?,?,?)", m.ID, scopeID(m.WorkspaceID), m.Category, m.Title, m.Summary, m.Content, m.Status, m.Version, doc, Now()); err != nil {
				return nil, op, Fail("conflict", "memory already exists: %s", m.ID)
			}
		} else {
			res, e := tx.Conn.ExecContext(ctx, "UPDATE memories SET category=?,title=?,summary=?,content=?,status=?,version=?,document=?,updated_at=? WHERE id=? AND workspace_id=? AND version=?", m.Category, m.Title, m.Summary, m.Content, m.Status, m.Version, doc, Now(), m.ID, scopeID(m.WorkspaceID), before)
			if e != nil {
				return nil, op, e
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return nil, op, Fail("conflict", "memory %s changed since version %d", m.ID, before)
			}
		}
		if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO revisions VALUES(?,?,?,?,?,?)", m.ID, m.Version, doc, Digest(m), op.ID, Now()); err != nil {
			return nil, op, err
		}
		if _, err = tx.Conn.ExecContext(ctx, "DELETE FROM memory_evidence WHERE memory_id=?", m.ID); err != nil {
			return nil, op, err
		}
		for _, e := range m.Evidence {
			if _, err = tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO memory_evidence VALUES(?,?)", m.ID, e.FragmentID); err != nil {
				return nil, op, err
			}
		}
	}
	return items, op, nil
}
func (tx *Tx) SetState(ctx context.Context, id, status, reason string, expected, revision int) (any, error) {
	if expected <= 0 {
		return nil, Fail("invalid_input", "expected-version is required")
	}
	current, err := ReadMemory(ctx, tx.Conn, id, tx.Request.Scope, 0)
	if err != nil {
		return nil, err
	}
	m := current
	if revision > 0 {
		m, err = ReadMemory(ctx, tx.Conn, id, tx.Request.Scope, revision)
		if err != nil {
			return nil, err
		}
	}
	m.Status = status
	if current.Version != expected {
		return nil, Fail("conflict", "expected version %d, current %d", expected, current.Version)
	}
	items, op, err := tx.SaveMemories(ctx, "memory."+status, reason, []Memory{m}, map[string]int{id: expected})
	if err != nil {
		return nil, err
	}
	return map[string]any{"memory": items[0], "operation": op}, nil
}
func History(ctx context.Context, q Queryer, id, scope string) ([]Memory, error) {
	if _, err := ReadMemory(ctx, q, id, scope, 0); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, "SELECT document FROM revisions WHERE memory_id=? ORDER BY version", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		var raw string
		var m Memory
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err = redactReadMemoryQuotes(ctx, q, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}
