CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);
CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE workspaces(id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT UNIQUE, created_at TEXT NOT NULL);
INSERT INTO workspaces VALUES('global','global',NULL,strftime('%Y-%m-%dT%H:%M:%fZ','now'));
CREATE TABLE sources(
 id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), kind TEXT NOT NULL,
 content TEXT NOT NULL, digest TEXT NOT NULL, observed_at TEXT NOT NULL DEFAULT '', captured_at TEXT NOT NULL,
 lineage TEXT NOT NULL DEFAULT '', redacted INTEGER NOT NULL DEFAULT 0,
 UNIQUE(workspace_id,digest)
);
CREATE TABLE source_locations(source_id TEXT NOT NULL REFERENCES sources(id), uri TEXT NOT NULL, PRIMARY KEY(source_id,uri));
CREATE TABLE fragments(
 id TEXT PRIMARY KEY, source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
 locator TEXT NOT NULL, content TEXT NOT NULL, digest TEXT NOT NULL, UNIQUE(source_id,locator)
);
CREATE TABLE migration_runs(id TEXT PRIMARY KEY, name TEXT NOT NULL, status TEXT NOT NULL, manifest_digest TEXT NOT NULL,
 schema_version INTEGER NOT NULL, prompt_version TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE migration_sources(run_id TEXT NOT NULL REFERENCES migration_runs(id), source_id TEXT NOT NULL REFERENCES sources(id),
 disposition TEXT NOT NULL DEFAULT 'pending', reason TEXT NOT NULL DEFAULT '', candidate_ids TEXT NOT NULL DEFAULT '[]',
 PRIMARY KEY(run_id,source_id));
CREATE TABLE candidates(
 id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), action TEXT NOT NULL,
 target_id TEXT NOT NULL DEFAULT '', expected_version INTEGER NOT NULL DEFAULT 0,
 document TEXT NOT NULL, digest TEXT NOT NULL, status TEXT NOT NULL, reason TEXT NOT NULL,
 run_id TEXT REFERENCES migration_runs(id), consolidated INTEGER NOT NULL DEFAULT 0,
 generator TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, applied_memory_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE reviews(id TEXT PRIMARY KEY, candidate_id TEXT NOT NULL REFERENCES candidates(id), candidate_digest TEXT NOT NULL,
 reviewer TEXT NOT NULL, decision TEXT NOT NULL, issues TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE memories(
 id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), category TEXT NOT NULL,
 title TEXT NOT NULL, summary TEXT NOT NULL, content TEXT NOT NULL, status TEXT NOT NULL,
 version INTEGER NOT NULL, document TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX memories_scope ON memories(workspace_id,status);
CREATE TABLE revisions(memory_id TEXT NOT NULL REFERENCES memories(id), version INTEGER NOT NULL, document TEXT NOT NULL,
 digest TEXT NOT NULL, operation_id TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(memory_id,version));
CREATE TABLE memory_evidence(memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE, fragment_id TEXT NOT NULL REFERENCES fragments(id),
 PRIMARY KEY(memory_id,fragment_id));
CREATE TABLE relations(src_id TEXT NOT NULL, kind TEXT NOT NULL, dst_id TEXT NOT NULL, operation_id TEXT NOT NULL,
 PRIMARY KEY(src_id,kind,dst_id));
CREATE TABLE operations(id TEXT PRIMARY KEY, request_id TEXT NOT NULL, kind TEXT NOT NULL, actor TEXT NOT NULL,
 reason TEXT NOT NULL, changes TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE requests(id TEXT PRIMARY KEY, command TEXT NOT NULL, actor TEXT NOT NULL, ok INTEGER NOT NULL,
 error_code TEXT NOT NULL DEFAULT '', operation_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
CREATE TABLE idempotency(scope TEXT NOT NULL, command TEXT NOT NULL, key TEXT NOT NULL, request_hash TEXT NOT NULL,
 result TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(scope,command,key));
CREATE TABLE jobs(
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES migration_runs(id), kind TEXT NOT NULL,
 input TEXT NOT NULL, input_digest TEXT NOT NULL, status TEXT NOT NULL, worker TEXT NOT NULL DEFAULT '',
 lease_token TEXT NOT NULL DEFAULT '', lease_until TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0,
 output TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
 UNIQUE(run_id,kind,input_digest)
);
CREATE TABLE plans(id TEXT PRIMARY KEY, kind TEXT NOT NULL, document TEXT NOT NULL, digest TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', operation_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
CREATE TABLE tombstones(kind TEXT NOT NULL, fingerprint TEXT NOT NULL, operation_id TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(kind,fingerprint));
CREATE TABLE purge_runs(id TEXT PRIMARY KEY, status TEXT NOT NULL, manifest TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE VIRTUAL TABLE search_fts USING fts5(title,summary,content,kind UNINDEXED,ref_id UNINDEXED,workspace_id UNINDEXED,tokenize='trigram');
INSERT INTO search_fts(search_fts,rank) VALUES('secure-delete',1);
CREATE TRIGGER memory_insert AFTER INSERT ON memories BEGIN
 INSERT INTO search_fts(title,summary,content,kind,ref_id,workspace_id) VALUES(new.title,new.summary,new.content,'memory',new.id,new.workspace_id);
END;
CREATE TRIGGER memory_update AFTER UPDATE ON memories BEGIN
 DELETE FROM search_fts WHERE kind='memory' AND ref_id=old.id;
 INSERT INTO search_fts(title,summary,content,kind,ref_id,workspace_id) VALUES(new.title,new.summary,new.content,'memory',new.id,new.workspace_id);
END;
CREATE TRIGGER memory_delete AFTER DELETE ON memories BEGIN
 DELETE FROM search_fts WHERE kind='memory' AND ref_id=old.id;
END;
CREATE TRIGGER source_insert AFTER INSERT ON sources BEGIN
 INSERT INTO search_fts(title,summary,content,kind,ref_id,workspace_id) VALUES(new.kind,'',new.content,'source',new.id,new.workspace_id);
END;
CREATE TRIGGER source_update AFTER UPDATE ON sources BEGIN
 DELETE FROM search_fts WHERE kind='source' AND ref_id=old.id;
 INSERT INTO search_fts(title,summary,content,kind,ref_id,workspace_id) VALUES(new.kind,'',new.content,'source',new.id,new.workspace_id);
END;
CREATE TRIGGER source_delete AFTER DELETE ON sources BEGIN
 DELETE FROM search_fts WHERE kind='source' AND ref_id=old.id;
END;
