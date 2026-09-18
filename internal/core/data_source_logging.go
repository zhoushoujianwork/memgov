package core

import (
	"context"
	"database/sql"
	"errors"
)

func (tx *Tx) SetDataSourceLogHealth(ctx context.Context, id string, degraded bool) error {
	if _, err := ReadDataSource(ctx, tx.Conn, id); err != nil {
		return err
	}
	value := "healthy"
	if degraded {
		value = "degraded"
	}
	_, err := tx.Conn.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", "data-source.log-health."+id, value)
	return err
}
func DataSourceLogHealth(ctx context.Context, q Queryer, id string) (string, error) {
	var value string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", "data-source.log-health."+id).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "unknown", nil
	}
	return value, err
}
