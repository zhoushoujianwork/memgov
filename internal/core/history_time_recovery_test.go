package core

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDWSHistoryTimestampCorrectionRequiresFreshMatchingObservation(t *testing.T) {
	for _, scenario := range []string{"verified", "other_adapter", "other_sender", "changed_body", "offline_import", "no_marker", "wrong_delta"} {
		t.Run(scenario, func(t *testing.T) {
			s := testStore(t)
			c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
			e := sampleEvent("old-time", "immutable body", Sender{IDType: "union_id", IDValue: "alice"})
			e.Origin, e.ParseVersion, e.ProviderEventID = "history", "dws/1", ""
			e.SentAt, e.EventAt = "2026-09-14T18:00:00Z", "2026-09-14T18:00:00Z"
			old := intake(t, s, c.ID, e, "old-time")
			runtimeMutate(t, s, "test.old.observed", func(tx *Tx) (any, error) {
				return nil, tx.ObserveMessage(context.Background(), c.ID, e.ConversationID, e.SentAt)
			})
			e.ParseVersion = "dws-history/2"
			e.HistoryPreviousSentAt = e.SentAt
			e.SentAt, e.EventAt = "2026-09-14T10:00:00Z", "2026-09-14T10:00:00Z"
			switch scenario {
			case "other_adapter":
				e.Adapter = "app"
			case "other_sender":
				e.Sender.IDValue = "mallory"
			case "changed_body":
				e.Body = "changed"
			case "offline_import":
				e.Origin = "import"
			case "no_marker":
				e.HistoryPreviousSentAt = ""
			case "wrong_delta":
				e.HistoryPreviousSentAt = "2026-09-14T19:00:00Z"
			}
			err := intakeErr(t, s, c.ID, e, "fresh-time")
			if scenario == "wrong_delta" {
				if ErrorCode(err) != "invalid_input" {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var sentAt string
			var corrected int
			s.DB.QueryRow("SELECT sent_at FROM messages WHERE id=?", old.MessageID).Scan(&sentAt)
			s.DB.QueryRow("SELECT count(*) FROM operations WHERE kind='message.history_time.correct'").Scan(&corrected)
			want, count := "2026-09-14T18:00:00Z", 0
			if scenario == "verified" {
				want, count = e.SentAt, 1
			}
			if sentAt != want || corrected != count {
				t.Fatalf("time=%s audit=%d", sentAt, corrected)
			}
			if scenario == "verified" {
				var reason string
				s.DB.QueryRow("SELECT reason FROM operations WHERE kind='message.history_time.correct'").Scan(&reason)
				var audit map[string]string
				if json.Unmarshal([]byte(reason), &audit) != nil || audit["before"] != e.HistoryPreviousSentAt || audit["after"] != e.SentAt || audit["verified_inbox_event_id"] == "" {
					t.Fatalf("audit %s", reason)
				}
				mark, markErr := ReadWatermark(context.Background(), s.DB, c.ID, e.ConversationID)
				if markErr != nil || mark.ObservedAt != e.SentAt {
					t.Fatalf("incorrect observed watermark %+v %v", mark, markErr)
				}
				var revision int
				s.DB.QueryRow("SELECT current_revision FROM messages WHERE id=?", old.MessageID).Scan(&revision)
				if revision != 1 {
					t.Fatal("time correction rewrote content evidence")
				}
				intake(t, s, c.ID, e, "fresh-replay")
				s.DB.QueryRow("SELECT count(*) FROM operations WHERE kind='message.history_time.correct'").Scan(&corrected)
				if corrected != 1 {
					t.Fatal("correction was not idempotent")
				}
			}
		})
	}
}

func TestLegacyHistoryRetryReplaysFromFirstObservationAndKeepsFixedRange(t *testing.T) {
	s, source, h := historyFixture(t)
	ctx := context.Background()
	e := sampleEvent("first-old-time", "old body", Sender{IDType: "union_id", IDValue: "alice"})
	e.ParseVersion, e.Origin, e.ProviderEventID = "dws/1", "history", ""
	e.EventAt = e.SentAt
	intake(t, s, source.ChannelID, e, "first-time")
	oldCursor := JSON(map[string]any{"schema_version": 1, "kind": "dws_history_time", "conversation_id": "cid:group1", "start_at": h.StartAt, "end_at": h.EndAt, "resume_at": "2026-09-20T12:00:00Z"})
	runtimeMutate(t, s, "test.legacy.fail", func(tx *Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, "UPDATE history_imports SET status='failed',cursor=? WHERE id=?", oldCursor, h.ID)
		return nil, err
	})
	var retried HistoryImport
	runtimeMutate(t, s, "test.legacy.retry", func(tx *Tx) (any, error) {
		var err error
		retried, err = tx.RetryHistoryImport(ctx, h.ID)
		return retried, err
	})
	var cursor map[string]any
	if json.Unmarshal([]byte(retried.Cursor), &cursor) != nil || cursor["schema_version"] != float64(2) || cursor["resume_at"] != "2026-09-14T01:59:59Z" || retried.StartAt != h.StartAt || retried.EndAt != h.EndAt || retried.Status != "queued" {
		t.Fatalf("recovery %+v cursor=%+v", retried, cursor)
	}
	runtimeMutate(t, s, "test.legacy.cancel", func(tx *Tx) (any, error) { return tx.CancelHistoryImport(ctx, h.ID) })
	runtimeMutate(t, s, "test.legacy.retry.again", func(tx *Tx) (any, error) {
		again, err := tx.RetryHistoryImport(ctx, h.ID)
		if err == nil && again.Cursor != retried.Cursor {
			t.Fatal("v2 retry rewound again")
		}
		return again, err
	})
}

// Completing the watched import must not claim that old observations in other
// conversations were reverified. A separately authorized ordinary history read
// can repair those messages through Intake without adding a watched group/job.
func TestDWSHistoryTimeRepairAcrossWatchedAndUnwatchedConversations(t *testing.T) {
	s, source, _ := historyFixture(t)
	ctx := context.Background()
	runtimeMutate(t, s, "test.history.other_route", func(tx *Tx) (any, error) {
		return tx.AddRoute(ctx, source.ChannelID, RouteInput{ConversationID: "cid:other", ConversationType: "group"})
	})
	before, err := ReadDataSource(ctx, s.DB, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.RouteIDs) != 1 {
		t.Fatalf("expected only original watched route, got %d", len(before.RouteIDs))
	}
	oldEvent := func(id, conversation string) NormalizedEvent {
		e := sampleEvent(id, "unchanged evidence", Sender{IDType: "union_id", IDValue: "alice"})
		e.ConversationID = conversation
		e.Origin, e.ParseVersion, e.ProviderEventID = "history", "dws/1", ""
		e.SentAt, e.EventAt = "2026-09-15T10:45:03Z", "2026-09-15T10:45:03Z"
		return e
	}
	watched := oldEvent("watched", "cid:group1")
	other := oldEvent("other", "cid:other")
	aware := oldEvent("already-aware", "cid:other")
	watchedResult := intake(t, s, source.ChannelID, watched, "watched-old")
	otherResult := intake(t, s, source.ChannelID, other, "other-old")
	awareResult := intake(t, s, source.ChannelID, aware, "aware-old")
	correctedEvent := func(e NormalizedEvent) NormalizedEvent {
		e.ParseVersion = "dws-history/2"
		e.HistoryPreviousSentAt = e.SentAt
		e.SentAt, e.EventAt = "2026-09-15T02:45:03Z", "2026-09-15T02:45:03Z"
		return e
	}
	assertTime := func(id, want string) {
		t.Helper()
		var got string
		if err := s.DB.QueryRowContext(ctx, "SELECT sent_at FROM messages WHERE id=?", id).Scan(&got); err != nil || got != want {
			t.Fatalf("message time=%s want=%s err=%v", got, want, err)
		}
	}
	job := historyClaim(t, s, source.ID)
	done, err := historyFinish(t, s, job, []NormalizedEvent{correctedEvent(watched)}, true, "")
	if err != nil || done.Status != "completed" {
		t.Fatalf("history completion %+v %v", done, err)
	}
	assertTime(watchedResult.MessageID, "2026-09-15T02:45:03Z")
	assertTime(otherResult.MessageID, other.SentAt)
	assertTime(awareResult.MessageID, aware.SentAt)

	// Ordinary history intake, independently of the completed job, repairs the
	// exact reobserved message. Merely having dws/1 provenance is insufficient:
	// old DWS also supported already-qualified timestamps with no CST mistake.
	intake(t, s, source.ChannelID, correctedEvent(other), "other-verified")
	aware.ParseVersion = "dws-history/2"
	intake(t, s, source.ChannelID, aware, "aware-verified")
	assertTime(otherResult.MessageID, "2026-09-15T02:45:03Z")
	assertTime(awareResult.MessageID, aware.SentAt)
	var audits, jobs int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM operations WHERE kind='message.history_time.correct'").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM history_imports").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	after, err := ReadDataSource(ctx, s.DB, source.ID)
	if err != nil || audits != 2 || jobs != 1 || JSON(before.RouteIDs) != JSON(after.RouteIDs) {
		t.Fatalf("unexpected audit/scope change audits=%d jobs=%d err=%v", audits, jobs, err)
	}
}
