package core

import (
	"context"
	"strings"
	"testing"
)

// Schema 5 repairs a real defect: routes on the default private policy all shared
// the audience key "local_private", so one publication permitted disclosure to
// every conversation on every channel. The migration rescopes the keys and
// withdraws the ambiguous permissions instead of attributing them to a route.
func TestSchemaFiveRescopesAudienceKeysAndWithdrawsSharedPublications(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	// Reproduce the pre-migration state: the shared key on the route, a
	// publication under it, and a draft that was written against it.
	if _, err := s.DB.ExecContext(ctx, "UPDATE channel_routes SET audience_key='local_private' WHERE id=?", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO memory_publications(memory_id,audience_key,version,reason,created_at) VALUES(?,'local_private',?,?,?)",
		m.ID, m.Version, "旧的共享键披露", Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO outbox(id,channel_id,route_id,route_version,conversation_id,audience_key,input_digest,state,created_at,updated_at)
	 VALUES('draft-1',?,?,?,?,'local_private','digest-1','draft',?,?)`, c.ID, r.ID, r.Version, r.ConversationID, Now(), Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO request_contexts(id,channel_id,route_id,route_version,conversation_id,audience_key,workspace_id,expires_at,created_at)
	 VALUES('ctx-1',?,?,?,?,'local_private','',?,?)`, c.ID, r.ID, r.Version, r.ConversationID, "2999-01-01T00:00:00Z", Now()); err != nil {
		t.Fatal(err)
	}
	// Re-run the migration over that state. Replaying it on an already-migrated
	// database also proves it is idempotent.
	if _, err := s.DB.ExecContext(ctx, schemaV5); err != nil {
		t.Fatal(err)
	}
	a, err := AudienceFor(ctx, s.DB, c.Name, r.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if a.AudienceKey != "local_private:"+c.ID+":"+r.ConversationID {
		t.Fatalf("the audience key was not rescoped: %q", a.AudienceKey)
	}
	// The ambiguous permission is gone, so disclosure is refused until it is
	// published again for this exact conversation.
	d, err := CheckDisclosure(ctx, s.DB, a, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || !strings.Contains(strings.Join(d.Reasons, ";"), "publish it explicitly") {
		t.Fatalf("a shared-key publication survived the migration: %+v", d)
	}
	var state, expires string
	if err = s.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE id='draft-1'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "stale" {
		t.Fatalf("a draft written under the shared key is still %q", state)
	}
	if err = s.DB.QueryRowContext(ctx, "SELECT expires_at FROM request_contexts WHERE id='ctx-1'").Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires == "2999-01-01T00:00:00Z" {
		t.Fatal("a request context written under the shared key is still valid")
	}
}
