CREATE TABLE runtime_direct_sessions(
 id TEXT PRIMARY KEY,
 runtime_id TEXT NOT NULL REFERENCES runtime_configs(id),
 route_id TEXT NOT NULL REFERENCES channel_routes(id),
 created_at TEXT NOT NULL,
 closed_at TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX runtime_direct_active ON runtime_direct_sessions(runtime_id,route_id) WHERE closed_at='';
CREATE TABLE runtime_direct_turns(
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 task_id TEXT NOT NULL UNIQUE REFERENCES runtime_tasks(id),
 session_id TEXT NOT NULL REFERENCES runtime_direct_sessions(id),
 command TEXT NOT NULL DEFAULT ''
);
