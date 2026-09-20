ALTER TABLE runtime_direct_sessions ADD COLUMN native_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_direct_sessions ADD COLUMN native_policy_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_direct_sessions ADD COLUMN native_context_digest TEXT NOT NULL DEFAULT '';
