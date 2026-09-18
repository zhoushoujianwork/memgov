package core

import (
	"context"
	"strings"
)

type RuntimeActionProposal struct {
	AttemptID string `json:"attempt_id"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Payload   string `json:"payload"`
}

// This tool only prepares an operation. Executing it still requires the
// existing exact owner confirmation bound to its displayed target and payload.
func (tx *Tx) ProposeRuntimeAction(ctx context.Context, taskID string, in RuntimeActionProposal) (map[string]any, error) {
	t, err := ReadRuntimeTask(ctx, tx.Conn, taskID)
	if err != nil {
		return nil, err
	}
	c, err := ReadRuntime(ctx, tx.Conn, t.RuntimeID)
	if err != nil {
		return nil, err
	}
	if t.Status != "running" || c.ApplicationMode != "direct" {
		return nil, Fail("denied", "action proposals require a running direct turn")
	}
	if _, err = RuntimeOwnerDirectProcessingRoute(ctx, tx.Conn, c, t.RouteID); err != nil {
		return nil, err
	}
	if err = tx.CheckRuntimeAttemptPolicy(ctx, in.AttemptID, t.ID, t.Version); err != nil {
		return nil, err
	}
	if current, e := runtimeTaskMessagesCurrent(ctx, tx.Conn, t.ID); e != nil || !current {
		if e != nil {
			return nil, e
		}
		return nil, Fail("conflict", "action turn is no longer current")
	}
	if strings.TrimSpace(in.Kind) == "" || strings.TrimSpace(in.Target) == "" || strings.TrimSpace(in.Payload) == "" || len([]rune(in.Kind)) > 64 || len([]rune(in.Target)) > 2000 || len([]rune(in.Payload)) > 50000 {
		return nil, Fail("invalid_input", "action requires bounded kind, target and payload")
	}
	var existing string
	if err = tx.Conn.QueryRowContext(ctx, "SELECT coalesce((SELECT id FROM runtime_pending_actions WHERE task_id=? AND task_version=? AND kind=? AND target=? AND payload_digest=? AND status='pending' LIMIT 1),'')", t.ID, t.Version, in.Kind, in.Target, Hash([]byte(in.Payload))).Scan(&existing); err != nil {
		return nil, err
	}
	if existing != "" {
		return map[string]any{"action_id": existing, "status": "pending"}, nil
	}
	var n int
	if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM runtime_pending_actions WHERE task_id=? AND task_version=? AND status='pending'", t.ID, t.Version).Scan(&n); err != nil {
		return nil, err
	}
	if n >= 20 {
		return nil, Fail("invalid_input", "too many pending operations")
	}
	id, now := NewID(), Now()
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_pending_actions(id,task_id,task_version,kind,target,payload,payload_digest,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?)", id, t.ID, t.Version, in.Kind, in.Target, in.Payload, Hash([]byte(in.Payload)), now, now)
	return map[string]any{"action_id": id, "status": "pending"}, err
}
