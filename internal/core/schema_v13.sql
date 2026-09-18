-- Source-specific policy survives new group discovery and process restarts.
ALTER TABLE data_sources ADD COLUMN ignore_rules TEXT NOT NULL DEFAULT '[]';
ALTER TABLE data_sources ADD COLUMN member_robot_code TEXT NOT NULL DEFAULT '';
ALTER TABLE data_sources ADD COLUMN history_enabled INTEGER NOT NULL DEFAULT 1 CHECK(history_enabled IN (0,1));
ALTER TABLE data_sources ADD COLUMN history_days INTEGER NOT NULL DEFAULT 30 CHECK(history_days BETWEEN 1 AND 30);
ALTER TABLE data_sources ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1));
