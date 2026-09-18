-- Schema 2: DingTalk message channels, inbox, message versions, audience,
-- request contexts and the send outbox. Released v1 objects are never rewritten;
-- migration_runs only gains columns with defaults that keep existing rows valid.
ALTER TABLE migration_runs ADD COLUMN purpose TEXT NOT NULL DEFAULT 'migration';
ALTER TABLE migration_runs ADD COLUMN route_id TEXT NOT NULL DEFAULT '';

CREATE TABLE channels(
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL UNIQUE,
 kind TEXT NOT NULL,
 provider TEXT NOT NULL DEFAULT 'dingtalk',
 tenant TEXT NOT NULL,
 auth_namespace TEXT NOT NULL,
 id_namespace TEXT NOT NULL,
 identity TEXT NOT NULL DEFAULT '{}',
 credential_ref TEXT NOT NULL DEFAULT '',
 capabilities TEXT NOT NULL DEFAULT '{}',
 tool_version TEXT NOT NULL DEFAULT '',
 config_version INTEGER NOT NULL DEFAULT 1,
 status TEXT NOT NULL DEFAULT 'configured',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);

CREATE TABLE channel_routes(
 id TEXT PRIMARY KEY,
 channel_id TEXT NOT NULL REFERENCES channels(id),
 conversation_id TEXT NOT NULL,
 conversation_type TEXT NOT NULL DEFAULT 'group',
 workspace_id TEXT NOT NULL REFERENCES workspaces(id),
 mode TEXT NOT NULL DEFAULT 'collect',
 triggers TEXT NOT NULL DEFAULT '[]',
 audience_policy TEXT NOT NULL DEFAULT 'local_private',
 audience_key TEXT NOT NULL,
 memory_policy TEXT NOT NULL DEFAULT 'explicit_only',
 send_policy TEXT NOT NULL DEFAULT 'draft_only',
 approval_display TEXT NOT NULL DEFAULT 'display_only',
 retention TEXT NOT NULL DEFAULT '{}',
 version INTEGER NOT NULL DEFAULT 1,
 status TEXT NOT NULL DEFAULT 'active',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(channel_id,conversation_id)
);

CREATE TABLE principals(
 id TEXT PRIMARY KEY, tenant TEXT NOT NULL, id_type TEXT NOT NULL, id_value TEXT NOT NULL,
 created_at TEXT NOT NULL, UNIQUE(tenant,id_type,id_value)
);
-- A verified alias maps one platform identifier to a principal. Same display
-- name is never a mapping basis; unverified rows stay unusable for authorization.
CREATE TABLE identity_aliases(
 tenant TEXT NOT NULL, id_type TEXT NOT NULL, id_value TEXT NOT NULL,
 principal_id TEXT NOT NULL REFERENCES principals(id),
 basis TEXT NOT NULL, verified INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL,
 PRIMARY KEY(tenant,id_type,id_value)
);

CREATE TABLE conversations(
 id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES channels(id),
 provider_conversation_id TEXT NOT NULL, kind TEXT NOT NULL DEFAULT 'group',
 members_fresh_at TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
 UNIQUE(channel_id,provider_conversation_id)
);

CREATE TABLE inbox_events(
 id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES channels(id),
 dedupe_key TEXT NOT NULL UNIQUE,
 event_kind TEXT NOT NULL,
 provider_event_id TEXT NOT NULL DEFAULT '',
 provider_message_id TEXT NOT NULL DEFAULT '',
 conversation_id TEXT NOT NULL DEFAULT '',
 event_at TEXT NOT NULL DEFAULT '',
 received_at TEXT NOT NULL,
 payload TEXT NOT NULL DEFAULT '{}',
 payload_digest TEXT NOT NULL DEFAULT '',
 adapter TEXT NOT NULL DEFAULT '',
 parse_version TEXT NOT NULL DEFAULT '',
 origin TEXT NOT NULL DEFAULT 'stream',
 status TEXT NOT NULL DEFAULT 'received',
 error TEXT NOT NULL DEFAULT '',
 message_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX inbox_events_scan ON inbox_events(channel_id,status,received_at);

CREATE TABLE messages(
 id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES channels(id),
 message_key TEXT NOT NULL UNIQUE,
 conversation_id TEXT NOT NULL,
 provider_message_id TEXT NOT NULL,
 sender_principal TEXT NOT NULL DEFAULT '',
 sender_id_type TEXT NOT NULL DEFAULT '',
 sender_id_value TEXT NOT NULL DEFAULT '',
 sent_at TEXT NOT NULL DEFAULT '',
 current_revision INTEGER NOT NULL DEFAULT 1,
 availability TEXT NOT NULL DEFAULT 'available',
 availability_reason TEXT NOT NULL DEFAULT '',
 weak_identity INTEGER NOT NULL DEFAULT 0,
 self_authored INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX messages_conversation ON messages(channel_id,conversation_id,sent_at);

-- A content change creates a new revision. Original revisions are never
-- overwritten, and each revision owns at most one canonical Source snapshot.
CREATE TABLE message_revisions(
 message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,
 body TEXT NOT NULL DEFAULT '',
 body_digest TEXT NOT NULL DEFAULT '',
 format TEXT NOT NULL DEFAULT 'text',
 edited_at TEXT NOT NULL DEFAULT '',
 ordering TEXT NOT NULL DEFAULT '',
 snapshot TEXT NOT NULL DEFAULT '{}',
 source_id TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 PRIMARY KEY(message_id,revision)
);

CREATE TABLE message_observations(
 message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,
 channel_id TEXT NOT NULL,
 adapter TEXT NOT NULL,
 inbox_event_id TEXT NOT NULL DEFAULT '',
 observed_at TEXT NOT NULL,
 PRIMARY KEY(message_id,revision,channel_id,adapter,inbox_event_id)
);

CREATE TABLE message_relations(
 src_message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 kind TEXT NOT NULL,
 dst_provider_message_id TEXT NOT NULL,
 dst_message_id TEXT NOT NULL DEFAULT '',
 origin_sender TEXT NOT NULL DEFAULT '',
 confidence TEXT NOT NULL DEFAULT 'provider',
 PRIMARY KEY(src_message_id,kind,dst_provider_message_id)
);

CREATE TABLE message_attachments(
 message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL, ordinal INTEGER NOT NULL,
 name TEXT NOT NULL DEFAULT '', media_type TEXT NOT NULL DEFAULT '',
 resource_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'referenced',
 PRIMARY KEY(message_id,revision,ordinal)
);

-- A recall seen before its original message must keep that message unusable
-- when a later backfill supplies the body.
CREATE TABLE message_recalls(
 message_key TEXT PRIMARY KEY,
 recalled_at TEXT NOT NULL DEFAULT '',
 inbox_event_id TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL
);

CREATE TABLE source_origins(
 source_id TEXT PRIMARY KEY REFERENCES sources(id),
 channel_id TEXT NOT NULL, message_id TEXT NOT NULL, revision INTEGER NOT NULL,
 route_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
CREATE INDEX source_origins_message ON source_origins(message_id,revision);

CREATE TABLE source_availability(
 source_id TEXT PRIMARY KEY REFERENCES sources(id),
 state TEXT NOT NULL DEFAULT 'available',
 reason TEXT NOT NULL DEFAULT '',
 change_seq INTEGER NOT NULL DEFAULT 1,
 updated_at TEXT NOT NULL
);

-- Remote disclosure is an explicit local act bound to one memory version.
CREATE TABLE memory_publications(
 memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
 audience_key TEXT NOT NULL,
 version INTEGER NOT NULL,
 reason TEXT NOT NULL DEFAULT '',
 operation_id TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 PRIMARY KEY(memory_id,audience_key)
);

CREATE TABLE request_contexts(
 id TEXT PRIMARY KEY,
 channel_id TEXT NOT NULL REFERENCES channels(id),
 route_id TEXT NOT NULL REFERENCES channel_routes(id),
 route_version INTEGER NOT NULL,
 conversation_id TEXT NOT NULL,
 audience_key TEXT NOT NULL,
 workspace_id TEXT NOT NULL,
 principal_id TEXT NOT NULL DEFAULT '',
 sender_id_type TEXT NOT NULL DEFAULT '', sender_id_value TEXT NOT NULL DEFAULT '',
 trigger_message_id TEXT NOT NULL DEFAULT '',
 intent TEXT NOT NULL DEFAULT 'query',
 query TEXT NOT NULL DEFAULT '',
 origin TEXT NOT NULL DEFAULT 'stream',
 reply_transport TEXT NOT NULL DEFAULT '',
 reply_credential_expires_at TEXT NOT NULL DEFAULT '',
 expires_at TEXT NOT NULL,
 created_at TEXT NOT NULL
);

CREATE TABLE outbox(
 id TEXT PRIMARY KEY,
 channel_id TEXT NOT NULL REFERENCES channels(id),
 route_id TEXT NOT NULL REFERENCES channel_routes(id),
 route_version INTEGER NOT NULL,
 request_context_id TEXT NOT NULL DEFAULT '',
 job_id TEXT NOT NULL DEFAULT '',
 conversation_id TEXT NOT NULL,
 audience_key TEXT NOT NULL,
 sender_identity TEXT NOT NULL DEFAULT '',
 transport TEXT NOT NULL DEFAULT '',
 content TEXT NOT NULL DEFAULT '',
 format TEXT NOT NULL DEFAULT 'text',
 citations TEXT NOT NULL DEFAULT '[]',
 evidence_seq TEXT NOT NULL DEFAULT '{}',
 input_digest TEXT NOT NULL,
 display_digest TEXT NOT NULL DEFAULT '',
 send_policy TEXT NOT NULL DEFAULT 'draft_only',
 state TEXT NOT NULL DEFAULT 'draft',
 reason TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(route_id,input_digest)
);

CREATE TABLE delivery_attempts(
 id TEXT PRIMARY KEY,
 outbox_id TEXT NOT NULL REFERENCES outbox(id) ON DELETE CASCADE,
 attempt INTEGER NOT NULL,
 transport TEXT NOT NULL,
 idempotency_key TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL,
 receipt TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL DEFAULT ''
);

-- One live receiver per channel. A stale holder's writes are rejected by fence.
CREATE TABLE channel_leases(
 channel_id TEXT PRIMARY KEY REFERENCES channels(id),
 holder TEXT NOT NULL, token TEXT NOT NULL,
 fence INTEGER NOT NULL DEFAULT 1,
 until TEXT NOT NULL, updated_at TEXT NOT NULL
);

-- Coverage only advances when every page of a window succeeded. A truncated
-- window keeps its stop reason and stays outside the completed range.
CREATE TABLE coverage_windows(
 id TEXT PRIMARY KEY,
 channel_id TEXT NOT NULL REFERENCES channels(id),
 conversation_id TEXT NOT NULL,
 start_at TEXT NOT NULL, end_at TEXT NOT NULL,
 complete INTEGER NOT NULL DEFAULT 0,
 messages INTEGER NOT NULL DEFAULT 0,
 stop_reason TEXT NOT NULL DEFAULT '',
 cursor TEXT NOT NULL DEFAULT '',
 gap TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL
);
CREATE INDEX coverage_windows_scan ON coverage_windows(channel_id,conversation_id,start_at);

CREATE TABLE channel_watermarks(
 channel_id TEXT NOT NULL REFERENCES channels(id),
 conversation_id TEXT NOT NULL,
 observed_at TEXT NOT NULL DEFAULT '',
 covered_until TEXT NOT NULL DEFAULT '',
 gap_unresolved INTEGER NOT NULL DEFAULT 0,
 updated_at TEXT NOT NULL,
 PRIMARY KEY(channel_id,conversation_id)
);
