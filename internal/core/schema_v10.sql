CREATE TABLE applied_configs (
 id TEXT PRIMARY KEY,
 version INTEGER NOT NULL UNIQUE CHECK(version>0),
 schema_version INTEGER NOT NULL CHECK(schema_version>0),
 declaration TEXT NOT NULL,
 objects TEXT NOT NULL,
 digest TEXT NOT NULL,
 created_at TEXT NOT NULL
);
