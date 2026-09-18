-- Schema 3: unattended AI runtime. Runtime state is authoritative SQLite data;
-- operational JSONL logs remain an independent, disposable diagnostic stream.
CREATE TABLE runtime_configs(
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL UNIQUE,
 channel_id TEXT NOT NULL REFERENCES channels(id),
 route_ids TEXT NOT NULL DEFAULT '[]',
 delivery_route_id TEXT NOT NULL REFERENCES channel_routes(id),
 owner_principal_id TEXT NOT NULL REFERENCES principals(id),
 owner_id_type TEXT NOT NULL,
 owner_id_value TEXT NOT NULL,
 analysis_model TEXT NOT NULL DEFAULT 'haiku',
 execution_model TEXT NOT NULL DEFAULT '',
 agent_preset TEXT NOT NULL DEFAULT 'claude-default',
 item_threshold INTEGER NOT NULL DEFAULT 20,
 max_wait_seconds INTEGER NOT NULL DEFAULT 300,
 reconcile_seconds INTEGER NOT NULL DEFAULT 300,
 concurrency INTEGER NOT NULL DEFAULT 1,
 status TEXT NOT NULL DEFAULT 'configured',
 degraded_reason TEXT NOT NULL DEFAULT '',
 version INTEGER NOT NULL DEFAULT 1,
 bootstrap_at TEXT NOT NULL,
 last_started_at TEXT NOT NULL DEFAULT '',
 last_stopped_at TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);

CREATE TABLE runtime_message_states(
 runtime_id TEXT NOT NULL REFERENCES runtime_configs(id) ON DELETE CASCADE,
 route_id TEXT NOT NULL REFERENCES channel_routes(id),
 message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,
 state TEXT NOT NULL,
 batch_id TEXT NOT NULL DEFAULT '',
 first_seen_at TEXT NOT NULL,
 processed_at TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(runtime_id,message_id)
);
CREATE INDEX runtime_message_pending ON runtime_message_states(runtime_id,route_id,state,first_seen_at);

CREATE TABLE runtime_batches(
 id TEXT PRIMARY KEY,
 runtime_id TEXT NOT NULL REFERENCES runtime_configs(id) ON DELETE CASCADE,
 route_id TEXT NOT NULL REFERENCES channel_routes(id),
 status TEXT NOT NULL,
 input_digest TEXT NOT NULL,
 message_count INTEGER NOT NULL,
 model TEXT NOT NULL,
 attempt INTEGER NOT NULL DEFAULT 1,
 output TEXT NOT NULL DEFAULT '',
 error_code TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 started_at TEXT NOT NULL DEFAULT '',
 finished_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE runtime_batch_messages(
 batch_id TEXT NOT NULL REFERENCES runtime_batches(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,
 ordinal INTEGER NOT NULL,
 PRIMARY KEY(batch_id,message_id)
);

CREATE TABLE runtime_tasks(
 id TEXT PRIMARY KEY,
 runtime_id TEXT NOT NULL REFERENCES runtime_configs(id) ON DELETE CASCADE,
 route_id TEXT NOT NULL REFERENCES channel_routes(id),
 canonical_key TEXT NOT NULL,
 kind TEXT NOT NULL DEFAULT 'task',
 title TEXT NOT NULL,
 instructions TEXT NOT NULL,
 status TEXT NOT NULL,
 needs_clarification INTEGER NOT NULL DEFAULT 0,
 version INTEGER NOT NULL DEFAULT 1,
 candidate_id TEXT NOT NULL DEFAULT '',
 result TEXT NOT NULL DEFAULT '',
 result_summary TEXT NOT NULL DEFAULT '',
 error_code TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(runtime_id,canonical_key)
);
CREATE INDEX runtime_tasks_status ON runtime_tasks(runtime_id,status,updated_at);
CREATE TABLE runtime_task_messages(
 task_id TEXT NOT NULL REFERENCES runtime_tasks(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,
 role TEXT NOT NULL DEFAULT 'trigger',
 PRIMARY KEY(task_id,message_id)
);

CREATE TABLE runtime_attempts(
 id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL REFERENCES runtime_tasks(id) ON DELETE CASCADE,
 task_version INTEGER NOT NULL,
 status TEXT NOT NULL,
 model TEXT NOT NULL,
 preset_name TEXT NOT NULL DEFAULT '',
 preset_commit TEXT NOT NULL DEFAULT '',
 workspace_dir TEXT NOT NULL DEFAULT '',
 input_digest TEXT NOT NULL,
 output TEXT NOT NULL DEFAULT '',
 summary TEXT NOT NULL DEFAULT '',
 artifacts TEXT NOT NULL DEFAULT '[]',
 tool_kinds TEXT NOT NULL DEFAULT '[]',
 usage TEXT NOT NULL DEFAULT '{}',
 error_code TEXT NOT NULL DEFAULT '',
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX runtime_attempts_task ON runtime_attempts(task_id,started_at);

CREATE TABLE runtime_pending_actions(
 id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL REFERENCES runtime_tasks(id) ON DELETE CASCADE,
 task_version INTEGER NOT NULL,
 kind TEXT NOT NULL,
 target TEXT NOT NULL,
 payload TEXT NOT NULL,
 payload_digest TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending',
 confirmed_by TEXT NOT NULL DEFAULT '',
 confirmation_origin TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
CREATE INDEX runtime_actions_status ON runtime_pending_actions(task_id,status,created_at);
