package core

import "context"

type purgeReceipt struct{ ID, Manifest, CreatedAt string }
type purgeHistory struct {
	Operations []Operation
	Receipts   []purgeReceipt
}

func readPurgeHistory(ctx context.Context, q Queryer) (purgeHistory, error) {
	var h purgeHistory
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT o.id,o.request_id,o.kind,o.created_at FROM operations o JOIN tombstones t ON t.operation_id=o.id")
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var o Operation
		if err = rows.Scan(&o.ID, &o.RequestID, &o.Kind, &o.CreatedAt); err != nil {
			rows.Close()
			return h, err
		}
		o.Actor = "redacted"
		o.Reason = "sensitive payload removal"
		h.Operations = append(h.Operations, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return h, err
	}
	rows, err = q.QueryContext(ctx, "SELECT id,manifest,created_at FROM purge_runs")
	if err != nil {
		return h, err
	}
	defer rows.Close()
	for rows.Next() {
		var p purgeReceipt
		if err = rows.Scan(&p.ID, &p.Manifest, &p.CreatedAt); err != nil {
			return h, err
		}
		h.Receipts = append(h.Receipts, p)
	}
	return h, rows.Err()
}
func (tx *Tx) retainPurgeHistory(ctx context.Context, h purgeHistory) error {
	for _, o := range h.Operations {
		if _, err := tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO operations VALUES(?,?,?,?,?,?,?)", o.ID, o.RequestID, o.Kind, o.Actor, o.Reason, "[]", o.CreatedAt); err != nil {
			return err
		}
	}
	for _, p := range h.Receipts {
		if _, err := tx.Conn.ExecContext(ctx, "INSERT OR REPLACE INTO purge_runs VALUES(?,'database_compacted',?,?)", p.ID, p.Manifest, p.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}
