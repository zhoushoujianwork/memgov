-- Schema 20: optional colleague direct-message acquisition and bounded raw-text
-- retention. Existing sources stay direct-disabled after upgrade until an
-- applied configuration explicitly enables them.
ALTER TABLE data_sources ADD COLUMN direct_enabled INTEGER NOT NULL DEFAULT 0 CHECK(direct_enabled IN (0,1));
ALTER TABLE data_sources ADD COLUMN direct_enabled_at TEXT NOT NULL DEFAULT '';
ALTER TABLE data_sources ADD COLUMN backfill_after_enable INTEGER NOT NULL DEFAULT 1 CHECK(backfill_after_enable IN (0,1));
ALTER TABLE data_sources ADD COLUMN retention_days INTEGER NOT NULL DEFAULT 0 CHECK(retention_days BETWEEN 0 AND 30);
ALTER TABLE data_sources ADD COLUMN last_direct_received_at TEXT NOT NULL DEFAULT '';
ALTER TABLE data_sources ADD COLUMN direct_discovery_covered_until TEXT NOT NULL DEFAULT '';
ALTER TABLE data_sources ADD COLUMN last_retention_at TEXT NOT NULL DEFAULT '';
ALTER TABLE data_sources ADD COLUMN last_retention_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE data_sources ADD COLUMN retention_error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE coverage_windows ADD COLUMN resolved_at TEXT NOT NULL DEFAULT '';

-- A direct conversation is addressed by its real provider conversation ID.
-- Display names are searchable hints only; authorization and exact filtering
-- use the stable peer identifier recorded here.
CREATE TABLE direct_conversation_contacts(
 channel_id TEXT NOT NULL REFERENCES channels(id),
 conversation_id TEXT NOT NULL,
 peer_id_type TEXT NOT NULL DEFAULT '',
 peer_id_value TEXT NOT NULL DEFAULT '',
 display_name TEXT NOT NULL DEFAULT '',
 observed_at TEXT NOT NULL,
 PRIMARY KEY(channel_id,conversation_id)
);
CREATE INDEX direct_contacts_identity ON direct_conversation_contacts(channel_id,peer_id_type,peer_id_value);
