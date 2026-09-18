package core

import "context"

// InvalidateRuntimeConfigWork revokes pre-change execution and delivery rights.
// It is called in the same SQLite transaction as a managed configuration change.
// A send already crossing the platform boundary becomes unknown, never retried.
func (tx *Tx) InvalidateRuntimeConfigWork(ctx context.Context, runtimeID string) error {
	c, err := ReadRuntime(ctx, tx.Conn, runtimeID)
	if err != nil {
		return err
	}
	now := Now()
	rows, err := tx.Conn.QueryContext(ctx, "SELECT id FROM runtime_batches WHERE runtime_id=? AND status='analyzing'", c.ID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = tx.FailRuntimeBatch(ctx, id, "configuration_changed"); err != nil {
			return err
		}
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_tasks SET status='stale',version=version+1,error_code='configuration_changed',updated_at=? WHERE runtime_id=? AND status IN ('pending','running','clarification','blocked','awaiting_confirmation','failed','action_failed')", now, c.ID); err != nil {
		return err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_attempts SET status='stale',error_code='configuration_changed',finished_at=? WHERE status='running' AND task_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)", now, c.ID); err != nil {
		return err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='stale',updated_at=? WHERE status IN ('pending','confirmed') AND task_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)", now, c.ID); err != nil {
		return err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_pending_actions SET status='unknown',updated_at=? WHERE status='executing' AND task_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)", now, c.ID); err != nil {
		return err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_action_attempts SET status='unknown',error_code='configuration_changed',finished_at=? WHERE status='running' AND task_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)", now, c.ID); err != nil {
		return err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE outbox SET state='stale',reason=CASE WHEN reason IN ('runtime_receipt','runtime_processing_receipt','runtime_completion_receipt','runtime_failure_receipt','result','confirmation') THEN reason ELSE 'configuration_changed' END,updated_at=? WHERE state IN ('draft','ready') AND job_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)", now, c.ID); err != nil {
		return err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE outbox SET state='unknown',reason=CASE WHEN reason IN ('runtime_receipt','runtime_processing_receipt','runtime_completion_receipt','runtime_failure_receipt','result','confirmation') THEN reason ELSE 'configuration_changed' END,updated_at=? WHERE state='sending' AND job_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)", now, c.ID); err != nil {
		return err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE delivery_attempts SET state='unknown',finished_at=? WHERE state='sending' AND outbox_id IN (SELECT o.id FROM outbox o JOIN runtime_tasks t ON t.id=o.job_id WHERE t.runtime_id=?)", now, c.ID); err != nil {
		return err
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_message_actions SET state='unknown',detail='configuration_changed',updated_at=? WHERE state='sending' AND task_id IN (SELECT id FROM runtime_tasks WHERE runtime_id=?)", now, c.ID); err != nil {
		return err
	}
	return nil
}
