-- Complete, version-bound discovery receipts distinguish provider observation
-- from routes prefilled by local configuration. Chat bodies are not copied.
CREATE TABLE source_group_discoveries (
 source_id TEXT PRIMARY KEY REFERENCES data_sources(id),
 source_version INTEGER NOT NULL,
 channel_version INTEGER NOT NULL,
 robot_code TEXT NOT NULL,
 robot_name TEXT NOT NULL,
 groups_json TEXT NOT NULL,
 observed_at TEXT NOT NULL,
 valid INTEGER NOT NULL DEFAULT 1 CHECK(valid IN (0,1))
);
