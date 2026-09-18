ALTER TABLE runtime_tasks ADD COLUMN resume TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_attempts ADD COLUMN agent_session TEXT NOT NULL DEFAULT '{}';
