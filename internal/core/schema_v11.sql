-- Independent collection state; channel_leases remains the sole receive lease.
CREATE TABLE data_sources (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, channel_id TEXT NOT NULL UNIQUE REFERENCES channels(id),
 workspace_id TEXT NOT NULL REFERENCES workspaces(id), status TEXT NOT NULL DEFAULT 'stopped',
 reconcile_seconds INTEGER NOT NULL DEFAULT 300, route_ids TEXT NOT NULL DEFAULT '[]',
 reconcile_cursor INTEGER NOT NULL DEFAULT 0, last_reconciled_at TEXT NOT NULL DEFAULT '',
 last_error_code TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE runtime_data_sources (
 runtime_id TEXT PRIMARY KEY REFERENCES runtime_configs(id),
 data_source_id TEXT NOT NULL REFERENCES data_sources(id), created_at TEXT NOT NULL
);
