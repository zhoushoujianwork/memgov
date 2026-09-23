package core

import (
	"context"
	"sort"
)

type Tombstone struct {
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint"`
	OperationID string `json:"operation_id"`
	CreatedAt   string `json:"created_at"`
}

func keys(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func intersects(values []string, set map[string]bool) bool {
	for _, v := range values {
		if set[v] {
			return true
		}
	}
	return false
}
func checkpoint(ctx context.Context, q Queryer) error {
	var busy, log, done int
	if err := q.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &done); err != nil {
		return dbError(err)
	}
	if busy != 0 {
		return Fail("unavailable", "WAL checkpoint is busy; logical purge is complete, retry cleanup")
	}
	return nil
}
