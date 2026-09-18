-- Historical import checkpoints and message provenance are business state.
ALTER TABLE messages ADD COLUMN context_only INTEGER NOT NULL DEFAULT 0 CHECK(context_only IN (0,1));
CREATE TABLE history_imports (
 id TEXT PRIMARY KEY,
 data_source_id TEXT NOT NULL REFERENCES data_sources(id),
 channel_id TEXT NOT NULL REFERENCES channels(id),
 route_id TEXT NOT NULL REFERENCES channel_routes(id),
 start_at TEXT NOT NULL,
 end_at TEXT NOT NULL,
 cursor TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','completed','failed','cancelled')),
 attempts INTEGER NOT NULL DEFAULT 0,
 failures INTEGER NOT NULL DEFAULT 0,
 events INTEGER NOT NULL DEFAULT 0,
 applied INTEGER NOT NULL DEFAULT 0,
 duplicates INTEGER NOT NULL DEFAULT 0,
 lease_token TEXT NOT NULL DEFAULT '',
 lease_until TEXT NOT NULL DEFAULT '',
 next_attempt_at TEXT NOT NULL DEFAULT '',
 error_code TEXT NOT NULL DEFAULT '',
 stop_reason TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(data_source_id,route_id,start_at,end_at)
);
CREATE INDEX history_import_ready ON history_imports(data_source_id,status,next_attempt_at,created_at);
