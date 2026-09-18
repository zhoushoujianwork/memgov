-- Pin the immutable configuration epoch actually used by each execution.
-- Version zero denotes an independently configured legacy runtime.
ALTER TABLE runtime_attempts ADD COLUMN applied_config_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runtime_action_attempts ADD COLUMN applied_config_version INTEGER NOT NULL DEFAULT 0;
