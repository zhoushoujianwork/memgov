package core

import "testing"

// Only for reconstruction of released schemas in migration tests.
func dropSchema23(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		"ALTER TABLE runtime_direct_sessions DROP COLUMN native_context_digest",
		"ALTER TABLE runtime_direct_sessions DROP COLUMN native_policy_digest",
		"ALTER TABLE runtime_direct_sessions DROP COLUMN native_session_id",
		"DELETE FROM schema_migrations WHERE version=26",
		"DROP TABLE runtime_reviews",
		"ALTER TABLE runtime_work_leases DROP COLUMN model_activity_at",
		"ALTER TABLE runtime_work_leases DROP COLUMN runtime_policy_digest",
		"ALTER TABLE runtime_tasks DROP COLUMN memory_status",
		"ALTER TABLE runtime_tasks DROP COLUMN memory_error_code",
		"DELETE FROM schema_migrations WHERE version=25",
		"ALTER TABLE inbox_events DROP COLUMN source_time_raw",
		"DROP TABLE source_route_backoff",
		"DELETE FROM schema_migrations WHERE version=24",
		"DROP TABLE runtime_resource_locks",
		"DROP TABLE runtime_work_leases",
		"DROP INDEX runtime_batch_retry",
		"ALTER TABLE runtime_batches DROP COLUMN retry_key",
		"ALTER TABLE runtime_batches DROP COLUMN next_run_at",
		"ALTER TABLE runtime_message_states DROP COLUMN retry_count",
		"ALTER TABLE runtime_message_states DROP COLUMN next_run_at",
		"ALTER TABLE runtime_configs DROP COLUMN analysis_concurrency",
		"ALTER TABLE runtime_configs DROP COLUMN analysis_timeout_seconds",
		"ALTER TABLE runtime_configs DROP COLUMN execution_timeout_seconds",
		"ALTER TABLE runtime_configs DROP COLUMN review_timeout_seconds",
		"DELETE FROM schema_migrations WHERE version=23",
	} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}
