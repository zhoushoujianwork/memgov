-- Schema 9: separate runtime application modes and bind group Agents to an
-- explicitly authorized DWS history source and fixed capability set.
ALTER TABLE runtime_configs ADD COLUMN application_mode TEXT NOT NULL DEFAULT 'proactive';
ALTER TABLE runtime_configs ADD COLUMN context_channel_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_configs ADD COLUMN agent_capabilities TEXT NOT NULL DEFAULT '[]';
ALTER TABLE runtime_configs ADD COLUMN memory_scope TEXT NOT NULL DEFAULT 'owner_authorized';
