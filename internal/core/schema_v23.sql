ALTER TABLE runtime_configs ADD COLUMN analysis_concurrency INTEGER NOT NULL DEFAULT 8;
ALTER TABLE runtime_configs ADD COLUMN analysis_timeout_seconds INTEGER NOT NULL DEFAULT 120;
ALTER TABLE runtime_configs ADD COLUMN execution_timeout_seconds INTEGER NOT NULL DEFAULT 900;
ALTER TABLE runtime_configs ADD COLUMN review_timeout_seconds INTEGER NOT NULL DEFAULT 120;
ALTER TABLE runtime_batches ADD COLUMN retry_key TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_batches ADD COLUMN next_run_at TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_message_states ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runtime_message_states ADD COLUMN next_run_at TEXT NOT NULL DEFAULT '';
CREATE TABLE runtime_work_leases (
 id TEXT PRIMARY KEY,
 runtime_id TEXT NOT NULL REFERENCES runtime_configs(id),
 kind TEXT NOT NULL,
 route_id TEXT NOT NULL,
 task_id TEXT NOT NULL DEFAULT '',
 task_version INTEGER NOT NULL DEFAULT 0,
 policy_version INTEGER NOT NULL,
 owner TEXT NOT NULL,
 owner_pid INTEGER NOT NULL DEFAULT 0,
 owner_started TEXT NOT NULL DEFAULT '',
 process_pid INTEGER NOT NULL DEFAULT 0,
 process_started TEXT NOT NULL DEFAULT '',
 phase TEXT NOT NULL DEFAULT 'preparing',
 activity_at TEXT NOT NULL DEFAULT '',
 deadline_at TEXT NOT NULL,
 heartbeat_at TEXT NOT NULL,
 lease_until TEXT NOT NULL,
 released INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX runtime_work_active ON runtime_work_leases(kind,released,runtime_id,route_id);
CREATE INDEX runtime_batch_retry ON runtime_batches(runtime_id,route_id,retry_key,next_run_at);
CREATE TABLE runtime_resource_locks (
 root TEXT NOT NULL,
 lease_id TEXT NOT NULL REFERENCES runtime_work_leases(id),
 PRIMARY KEY(root,lease_id)
);
