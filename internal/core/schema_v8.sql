-- Schema 8: retain the trusted addressing bit from bot callbacks.
-- Group Agent consumers use this normalized field instead of reparsing the
-- provider payload. Direct messages are addressed by definition at intake.
ALTER TABLE messages ADD COLUMN addressed INTEGER NOT NULL DEFAULT 0;
