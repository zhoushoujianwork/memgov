-- Schema 27: knowledge moves to Agent Workspace files. A verified full database
-- archive must be published by upgradeSchema before this migration runs.
-- Message Sources remain internal evidence; operational history is retained.
UPDATE outbox SET state=CASE WHEN state='sending' THEN 'unknown' ELSE 'stale' END,
 reason='legacy memory references archived',updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE (json_array_length(citations)>0 OR job_id IN (SELECT id FROM jobs) OR job_id IN (SELECT id FROM runtime_tasks WHERE kind='memory')) AND state IN ('draft','ready','sending');
UPDATE delivery_attempts SET state='unknown' WHERE outbox_id IN (SELECT id FROM outbox WHERE state='unknown' AND reason='legacy memory references archived') AND state='sending';
UPDATE runtime_pending_actions SET status=CASE WHEN status='executing' THEN 'unknown' ELSE 'stale' END,
 updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE task_id IN (SELECT id FROM runtime_tasks WHERE kind='memory') AND status IN ('pending','confirmed','executing');
UPDATE runtime_action_attempts SET status='unknown',error_code='memory_system_removed',finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE task_id IN (SELECT id FROM runtime_tasks WHERE kind='memory') AND status='running';
UPDATE runtime_message_actions SET state=CASE WHEN state='sending' THEN 'unknown' ELSE 'stale' END,detail='memory_system_removed',updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE task_id IN (SELECT id FROM runtime_tasks WHERE kind='memory') AND state IN ('draft','ready','sending');
UPDATE runtime_attempts SET status='stale',error_code='memory_system_removed',finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE task_id IN (SELECT id FROM runtime_tasks WHERE kind='memory') AND status='running';
DELETE FROM runtime_resource_locks WHERE lease_id IN (SELECT id FROM runtime_work_leases WHERE phase='review' OR task_id IN (SELECT id FROM runtime_tasks WHERE kind='memory'));
UPDATE runtime_work_leases SET released=1 WHERE phase='review' OR task_id IN (SELECT id FROM runtime_tasks WHERE kind='memory');
UPDATE runtime_tasks SET status='cancelled',version=version+1,resume='',error_code='memory_system_removed',updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE kind='memory' AND status NOT IN ('completed','cancelled');
-- The transaction hook removes only retired knowledge command caches.
DROP TABLE runtime_reviews;
ALTER TABLE runtime_tasks DROP COLUMN candidate_id;
ALTER TABLE runtime_tasks DROP COLUMN memory_status;
ALTER TABLE runtime_tasks DROP COLUMN memory_error_code;
DROP TRIGGER memory_insert;
DROP TRIGGER memory_update;
DROP TRIGGER memory_delete;
DELETE FROM search_fts WHERE kind='memory';
DROP TABLE memory_publications;
DROP TABLE memory_evidence;
DROP TABLE revisions;
DROP TABLE reviews;
DROP TABLE candidates;
DROP TABLE relations;
DROP TABLE memories;
DROP TABLE jobs;
DROP TABLE migration_sources;
DROP TABLE migration_runs;
DROP TABLE plans;
DROP TABLE purge_runs;

-- A provider session may still contain removed knowledge. Begin fresh sessions.
UPDATE runtime_direct_sessions SET native_session_id='',native_policy_digest='',native_context_digest='';
UPDATE runtime_tasks SET resume='';
