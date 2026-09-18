-- Positive group proofs and full-scope negative evidence are distinct.
ALTER TABLE source_group_discoveries ADD COLUMN complete INTEGER NOT NULL DEFAULT 0;
