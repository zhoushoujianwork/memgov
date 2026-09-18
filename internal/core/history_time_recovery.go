package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// correctDWSHistoryTime requires a newly received trusted DWS observation of the
// exact stored message and its known +8h parsing error. Old Source snapshots are
// immutable evidence; the correction is a separate audited fact.
func (tx *Tx) correctDWSHistoryTime(ctx context.Context, c Channel, e NormalizedEvent, result IntakeResult) error {
	if c.Kind != ChannelDwsPersonal || e.Adapter != "dws" || e.ParseVersion != "dws-history/2" || e.Origin != "history" || e.HistoryPreviousSentAt == "" || result.MessageID == "" || result.NewRevision {
		return nil
	}
	old, err := time.Parse(time.RFC3339Nano, e.HistoryPreviousSentAt)
	fresh, freshErr := time.Parse(time.RFC3339Nano, e.SentAt)
	if err != nil || freshErr != nil || !old.Equal(fresh.Add(8*time.Hour)) {
		return Fail("invalid_input", "invalid DWS history timestamp correction")
	}
	var stored string
	err = tx.Conn.QueryRowContext(ctx, `SELECT m.sent_at FROM messages m JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision
WHERE m.id=? AND m.channel_id=? AND m.conversation_id=? AND m.provider_message_id=? AND mr.body_digest=? AND m.sender_id_type=? AND m.sender_id_value=?
AND EXISTS(SELECT 1 FROM inbox_events ie WHERE ie.message_id=m.id AND ie.channel_id=m.channel_id AND ie.adapter='dws' AND ie.parse_version='dws/1' AND ie.origin='history' AND ie.event_at=m.sent_at)`, result.MessageID, c.ID, e.ConversationID, e.ProviderMessageID, Hash([]byte(e.Body)), e.Sender.IDType, e.Sender.IDValue).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if stored != e.HistoryPreviousSentAt {
		return nil
	}
	if _, err = tx.Conn.ExecContext(ctx, "UPDATE messages SET sent_at=?,updated_at=? WHERE id=? AND sent_at=?", e.SentAt, Now(), result.MessageID, stored); err != nil {
		return err
	}
	// If this exact faulty timestamp supplied the observed watermark, rebuild
	// that observation from persisted messages. CoveredUntil is independent.
	if _, err = tx.Conn.ExecContext(ctx, `UPDATE channel_watermarks SET observed_at=(SELECT coalesce(max(sent_at),'') FROM messages WHERE channel_id=? AND conversation_id=?),updated_at=? WHERE channel_id=? AND conversation_id=? AND observed_at=?`, c.ID, e.ConversationID, Now(), c.ID, e.ConversationID, stored); err != nil {
		return err
	}
	_, err = tx.Audit(ctx, "message.history_time.correct", JSON(map[string]string{"before": stored, "after": e.SentAt, "verified_inbox_event_id": result.InboxEventID, "basis": "dws_v1_cst8_display_time"}), objectChange("message", result.MessageID))
	return err
}

func (tx *Tx) recoverLegacyHistoryCursor(ctx context.Context, h HistoryImport) (string, error) {
	var cursor struct {
		SchemaVersion  int    `json:"schema_version"`
		Kind           string `json:"kind"`
		ConversationID string `json:"conversation_id"`
		StartAt        string `json:"start_at"`
		EndAt          string `json:"end_at"`
		ResumeAt       string `json:"resume_at"`
	}
	if json.Unmarshal([]byte(h.Cursor), &cursor) != nil || cursor.Kind != "dws_history_time" || cursor.SchemaVersion != 1 {
		return h.Cursor, nil
	}
	c, err := ReadChannel(ctx, tx.Conn, h.ChannelID)
	if err != nil {
		return "", err
	}
	r, err := ReadRoute(ctx, tx.Conn, h.RouteID)
	if err != nil {
		return "", err
	}
	if c.Kind != ChannelDwsPersonal || cursor.ConversationID != r.ConversationID || cursor.StartAt != h.StartAt || cursor.EndAt != h.EndAt {
		return "", Fail("invalid_input", "legacy history cursor does not match its import")
	}
	start, _ := time.Parse(time.RFC3339Nano, h.StartAt)
	resume := start
	// Re-read from the earliest prior DWS observation, not merely the last
	// cursor: every v1 advance may have skipped eight hours. Empty time before
	// the first proven observation does not need to be fetched again.
	var earliest sql.NullString
	err = tx.Conn.QueryRowContext(ctx, `SELECT min(ie.event_at) FROM inbox_events ie WHERE ie.channel_id=? AND ie.conversation_id=? AND ie.adapter='dws' AND ie.parse_version='dws/1' AND ie.origin='history' AND julianday(ie.event_at)>=julianday(?) AND julianday(ie.event_at)<julianday(?)`, h.ChannelID, r.ConversationID, h.StartAt, h.EndAt).Scan(&earliest)
	if err != nil {
		return "", err
	}
	if earliest.Valid {
		if observed, e := time.Parse(time.RFC3339Nano, earliest.String); e == nil && observed.Add(-8*time.Hour-time.Second).After(start) {
			resume = observed.Add(-8*time.Hour - time.Second)
		}
	}
	cursor.SchemaVersion, cursor.ResumeAt = 2, resume.UTC().Format(time.RFC3339Nano)
	if _, err = tx.Audit(ctx, "history.cursor.recover", JSON(map[string]string{"old_resume_at": cursorBefore(h.Cursor), "replay_from": cursor.ResumeAt, "basis": "legacy_dws_history_time_verification"}), objectChange("history_import", h.ID)); err != nil {
		return "", err
	}
	return JSON(cursor), nil
}

func cursorBefore(raw string) string {
	var value struct {
		ResumeAt string `json:"resume_at"`
	}
	_ = json.Unmarshal([]byte(raw), &value)
	return value.ResumeAt
}
