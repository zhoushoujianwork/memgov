package core

import (
	"context"
	"testing"
)

func TestSchemaSevenClosesOnlyLegacySourceCompleteWindows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var c Channel
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "channel.add"}, func(tx *Tx) (any, error) {
		var addErr error
		c, addErr = tx.AddChannel(ctx, ChannelInput{Name: "coverage", Kind: ChannelDwsPersonal,
			Identity: ChannelIdentity{Profile: "corp:user", ExpectedCorpID: "corp", ExpectedUserID: "user"}})
		return c, addErr
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id, conversation, reason, cursor string
	}{
		{"legacy-complete", "group-a", "source_complete", ""},
		{"legacy-only", "group-c", "source_complete", ""},
		{"real-failure", "group-a", "read_failed", ""},
		{"continued", "group-b", "source_complete", "next-page"},
	} {
		if _, err = s.DB.ExecContext(ctx, `INSERT INTO coverage_windows
(id,channel_id,conversation_id,start_at,end_at,complete,messages,stop_reason,cursor,gap,created_at)
VALUES(?,?,?,'2026-09-01T00:00:00Z','2026-09-01T01:00:00Z',0,0,?,?,?,?)`,
			row.id, c.ID, row.conversation, row.reason, row.cursor, "legacy gap", Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, conversation := range []string{"group-a", "group-b", "group-c"} {
		if _, err = s.DB.ExecContext(ctx, `INSERT INTO channel_watermarks
(channel_id,conversation_id,observed_at,covered_until,gap_unresolved,updated_at) VALUES(?,?, '', '',1,?)`, c.ID, conversation, Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.DB.ExecContext(ctx, schemaV7); err != nil {
		t.Fatal(err)
	}
	var complete int
	var gap string
	if err = s.DB.QueryRowContext(ctx, "SELECT complete,gap FROM coverage_windows WHERE id='legacy-complete'").Scan(&complete, &gap); err != nil || complete != 1 || gap != "" {
		t.Fatalf("legacy source-complete window was not closed: complete=%d gap=%q err=%v", complete, gap, err)
	}
	for _, id := range []string{"real-failure", "continued"} {
		if err = s.DB.QueryRowContext(ctx, "SELECT complete FROM coverage_windows WHERE id=?", id).Scan(&complete); err != nil || complete != 0 {
			t.Fatalf("real gap %s was incorrectly closed: complete=%d err=%v", id, complete, err)
		}
	}
	var unresolved int
	if err = s.DB.QueryRowContext(ctx, "SELECT gap_unresolved FROM channel_watermarks WHERE channel_id=? AND conversation_id='group-a'", c.ID).Scan(&unresolved); err != nil || unresolved != 1 {
		t.Fatalf("remaining failure was hidden: unresolved=%d err=%v", unresolved, err)
	}
	if err = s.DB.QueryRowContext(ctx, "SELECT gap_unresolved FROM channel_watermarks WHERE channel_id=? AND conversation_id='group-c'", c.ID).Scan(&unresolved); err != nil || unresolved != 0 {
		t.Fatalf("repaired conversation stayed unresolved: unresolved=%d err=%v", unresolved, err)
	}
}
