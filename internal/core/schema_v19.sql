ALTER TABLE runtime_configs ADD COLUMN agent_bash INTEGER NOT NULL DEFAULT 0 CHECK(agent_bash IN (0, 1));
ALTER TABLE runtime_configs ADD COLUMN external_actions TEXT NOT NULL DEFAULT 'owner_confirmation';
-- Owner private chat uses the built-in full CLI policy. Route and sender
-- verification is still mandatory before execution; other modes stay closed.
UPDATE runtime_configs SET agent_bash=1, external_actions='owner_request'
WHERE application_mode='direct';
