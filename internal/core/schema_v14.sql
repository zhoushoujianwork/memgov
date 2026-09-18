-- Cross-transport equivalence exists only after an authoritative platform
-- lookup proves both observations refer to the same logical event.
CREATE TABLE verified_message_associations(
 id TEXT PRIMARY KEY,
 tenant TEXT NOT NULL,
 first_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
 second_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
 proof_issuer TEXT NOT NULL,
 proof_digest TEXT NOT NULL,
 claimed_message_id TEXT NOT NULL DEFAULT '',
 claimed_runtime_id TEXT NOT NULL DEFAULT '',
 claimed_batch_id TEXT NOT NULL DEFAULT '',
 verified_at TEXT NOT NULL,
 claimed_at TEXT NOT NULL DEFAULT '',
 CHECK(first_message_id < second_message_id),
 CHECK(first_message_id <> second_message_id),
 CHECK(proof_issuer = 'platform_lookup')
);
CREATE INDEX verified_message_associations_claim ON verified_message_associations(claimed_message_id);
