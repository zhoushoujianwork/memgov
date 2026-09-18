ALTER TABLE inbox_events ADD COLUMN source_time_raw TEXT NOT NULL DEFAULT '';
CREATE TABLE source_route_backoff (
 source_id TEXT NOT NULL REFERENCES data_sources(id),
 route_id TEXT NOT NULL REFERENCES channel_routes(id),
 failures INTEGER NOT NULL DEFAULT 0,
 next_run_at TEXT NOT NULL DEFAULT '',
 error_code TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(source_id,route_id)
);
