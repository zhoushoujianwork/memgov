-- Schema 4: durable execution records for owner-confirmed external actions.
CREATE TABLE runtime_action_attempts(
 id TEXT PRIMARY KEY,
 action_id TEXT NOT NULL REFERENCES runtime_pending_actions(id) ON DELETE CASCADE,
 task_id TEXT NOT NULL REFERENCES runtime_tasks(id) ON DELETE CASCADE,
 task_version INTEGER NOT NULL,
 status TEXT NOT NULL,
 model TEXT NOT NULL DEFAULT '',
 workspace_dir TEXT NOT NULL DEFAULT '',
 result TEXT NOT NULL DEFAULT '',
 summary TEXT NOT NULL DEFAULT '',
 tool_kinds TEXT NOT NULL DEFAULT '[]',
 error_code TEXT NOT NULL DEFAULT '',
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX runtime_action_attempts_action ON runtime_action_attempts(action_id,started_at);
