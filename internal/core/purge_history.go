package core

import "context"

type purgeHistory struct {
	Operations []Operation
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
	return h, nil
}
func (tx *Tx) retainPurgeHistory(ctx context.Context, h purgeHistory) error {
	for _, o := range h.Operations {
		if _, err := tx.Conn.ExecContext(ctx, "INSERT OR IGNORE INTO operations VALUES(?,?,?,?,?,?,?)", o.ID, o.RequestID, o.Kind, o.Actor, o.Reason, "[]", o.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}
