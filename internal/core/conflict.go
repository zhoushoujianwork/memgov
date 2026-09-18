package core

import "context"

// Conflict links bind both current versions. Recording a conflict withdraws
// both claims from default recall; resolving the link does not itself prove
// either claim valid, so reactivation remains an explicit reviewed operation.
func (tx *Tx) MemoryConflict(ctx context.Context, a, b string, av, bv int, resolve bool, reason string) (any, error) {
	if a == b || av < 1 || bv < 1 {
		return nil, Fail("invalid_input", "two distinct memories and their current versions are required")
	}
	first, err := ReadMemory(ctx, tx.Conn, a, tx.Request.Scope, 0)
	if err != nil {
		return nil, err
	}
	second, err := ReadMemory(ctx, tx.Conn, b, tx.Request.Scope, 0)
	if err != nil {
		return nil, err
	}
	if first.Version != av || second.Version != bv {
		return nil, Fail("conflict", "one of the memory versions changed")
	}
	for _, m := range []Memory{first, second} {
		if scopeID(m.WorkspaceID) != scopeID(tx.Request.Scope) {
			return nil, Fail("denied", "conflicts require one explicit workspace")
		}
		if m.Status != "active" && m.Status != "disputed" {
			return nil, Fail("conflict", "conflict participants must be active or disputed")
		}
	}
	if a > b {
		a, b = b, a
	}
	var existing int
	if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM relations WHERE src_id=? AND kind='conflicts_with' AND dst_id=?", a, b).Scan(&existing); err != nil {
		return nil, err
	}
	if resolve && existing == 0 {
		return nil, Fail("not_found", "conflict link does not exist")
	}
	if !resolve && existing > 0 {
		return nil, Fail("conflict", "conflict already exists")
	}
	kind := "memory.conflict"
	if resolve {
		kind = "memory.conflict.resolve"
	} else {
		first.Status = "disputed"
		second.Status = "disputed"
	}
	items, op, err := tx.SaveMemories(ctx, kind, reason, []Memory{first, second}, map[string]int{first.ID: av, second.ID: bv})
	if err != nil {
		return nil, err
	}
	if resolve {
		_, err = tx.Conn.ExecContext(ctx, "DELETE FROM relations WHERE src_id=? AND kind='conflicts_with' AND dst_id=?", a, b)
	} else {
		_, err = tx.Conn.ExecContext(ctx, "INSERT INTO relations VALUES(?,'conflicts_with',?,?)", a, b, op.ID)
	}
	return map[string]any{"memories": items, "operation": op, "resolved": resolve}, err
}
