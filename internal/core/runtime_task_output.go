package core

import "context"

// Output can quote the original request, so it shares source and clear barriers.
func RuntimeTaskOutputCurrent(ctx context.Context, q Queryer, taskID string) (bool, error) {
	var count int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM runtime_task_messages WHERE task_id=?", taskID).Scan(&count); err != nil {
		return false, err
	}
	if count == 0 {
		return false, nil
	}
	return runtimeTaskMessagesCurrent(ctx, q, taskID)
}
