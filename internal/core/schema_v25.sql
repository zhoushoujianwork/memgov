ALTER TABLE runtime_tasks ADD COLUMN memory_status TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_tasks ADD COLUMN memory_error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_work_leases ADD COLUMN model_activity_at TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_work_leases ADD COLUMN runtime_policy_digest TEXT NOT NULL DEFAULT '';
CREATE TABLE runtime_reviews (
 id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL REFERENCES runtime_tasks(id),
 task_version INTEGER NOT NULL,
 runtime_id TEXT NOT NULL REFERENCES runtime_configs(id),
 source_attempt_id TEXT NOT NULL,
 candidate_input TEXT NOT NULL,
 candidate_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'pending',
 execution_id TEXT NOT NULL DEFAULT '',
 deadline_at TEXT NOT NULL,
 created_at TEXT NOT NULL,
 started_at TEXT NOT NULL DEFAULT '',
 finished_at TEXT NOT NULL DEFAULT '',
 error_code TEXT NOT NULL DEFAULT '',
 usage TEXT NOT NULL DEFAULT '{}',
 UNIQUE(task_id,task_version)
);
CREATE INDEX runtime_review_queue ON runtime_reviews(status,created_at);
