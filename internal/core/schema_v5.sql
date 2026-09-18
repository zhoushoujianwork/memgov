-- Schema 5: give every route its own disclosure boundary.
--
-- Until now a route with the default audience_policy='local_private' was keyed by
-- the bare policy name, so all such routes shared one audience_key. A single
-- publication therefore permitted disclosure to every conversation on every
-- channel, which is the opposite of the intended default. Keys are now scoped to
-- policy, channel and conversation.
UPDATE channel_routes SET audience_key = audience_policy || ':' || channel_id || ':' || conversation_id;

-- An existing publication under the shared key cannot be attributed to one
-- conversation after the fact, so it is withdrawn rather than guessed onto a
-- route. Disclosure returns to refused until it is published again explicitly,
-- which is the safe direction for a permission.
DELETE FROM memory_publications WHERE audience_key NOT LIKE '%:%';

-- Drafts and request contexts written under a shared key no longer describe a
-- real audience. Pending drafts are staled so they are re-checked instead of
-- spending a permission that has been withdrawn; contexts are expired so a reply
-- is opened under the current route.
UPDATE outbox SET state='stale', updated_at=created_at WHERE state IN ('draft','ready') AND audience_key NOT LIKE '%:%';
UPDATE request_contexts SET expires_at=created_at WHERE audience_key NOT LIKE '%:%';
