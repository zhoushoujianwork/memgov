-- Schema 6: select a local shell alias as the Claude environment profile.
-- Only the alias name is durable; credentials remain in the user's shell config.
ALTER TABLE runtime_configs ADD COLUMN claude_profile TEXT NOT NULL DEFAULT '';
