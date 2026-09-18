-- Agent-requested owner communications are independent of runtime completion
-- notifications. A durable sending row precedes the external call.
CREATE TABLE runtime_message_actions (
 id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL REFERENCES runtime_tasks(id),
 task_version INTEGER NOT NULL,
 attempt_id TEXT NOT NULL REFERENCES runtime_attempts(id),
 channel_id TEXT NOT NULL REFERENCES channels(id),
 channel_version INTEGER NOT NULL,
 owner_profile TEXT NOT NULL,
 owner_tenant TEXT NOT NULL,
 owner_user_id TEXT NOT NULL,
 target_type TEXT NOT NULL CHECK(target_type IN ('group','user')),
 target_id TEXT NOT NULL,
 content TEXT NOT NULL,
 reason TEXT NOT NULL,
 evidence_message_ids TEXT NOT NULL DEFAULT '[]',
 evidence_message_revisions TEXT NOT NULL DEFAULT '{}',
 input_digest TEXT NOT NULL,
 idempotency_key TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('sending','accepted','failed','unknown')),
 receipt TEXT NOT NULL DEFAULT '',
 detail TEXT NOT NULL DEFAULT '',
 provider_message_ids TEXT NOT NULL DEFAULT '[]',
 provider_send_task_id TEXT NOT NULL DEFAULT '',
 provider_conversation_id TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(channel_id,idempotency_key)
);
CREATE INDEX runtime_message_actions_task ON runtime_message_actions(task_id,created_at);

-- Stop only legacy runtime-generated background notifications. Execution and
-- accepted delivery history remain intact; uncertain handoffs stay uncertain.
UPDATE delivery_attempts SET state='unknown',finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE state='sending' AND outbox_id IN (
 SELECT o.id FROM outbox o JOIN runtime_tasks t ON t.id=o.job_id
 JOIN runtime_configs c ON c.id=t.runtime_id WHERE c.application_mode='proactive'
);
UPDATE outbox SET state=CASE WHEN state='sending' THEN 'unknown' ELSE 'stale' END,
 updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE state IN ('draft','ready','sending','retryable_failed') AND job_id IN (
 SELECT t.id FROM runtime_tasks t JOIN runtime_configs c ON c.id=t.runtime_id
 WHERE c.application_mode='proactive'
);
