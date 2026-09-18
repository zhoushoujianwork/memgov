package core

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Watermark separates what was seen from what was actually covered. observed_at
// is the newest message this channel saw; covered_until only advances over a
// range that was read completely. A gap that could not be closed stays visible.
type Watermark struct {
	ChannelID      string `json:"channel_id"`
	ConversationID string `json:"conversation_id"`
	ObservedAt     string `json:"observed_at,omitempty"`
	CoveredUntil   string `json:"covered_until,omitempty"`
	GapUnresolved  bool   `json:"gap_unresolved"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

func ReadWatermark(ctx context.Context, q Queryer, channelID, conversationID string) (Watermark, error) {
	w := Watermark{ChannelID: channelID, ConversationID: conversationID}
	var gap int
	err := q.QueryRowContext(ctx, "SELECT observed_at,covered_until,gap_unresolved,updated_at FROM channel_watermarks WHERE channel_id=? AND conversation_id=?",
		channelID, conversationID).Scan(&w.ObservedAt, &w.CoveredUntil, &gap, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return w, nil
	}
	w.GapUnresolved = gap == 1
	return w, err
}

// ObserveMessage advances the observed watermark only. Seeing a live event says
// nothing about whether the range before it was covered.
func (tx *Tx) ObserveMessage(ctx context.Context, channelID, conversationID, sentAt string) error {
	if sentAt == "" {
		return nil
	}
	_, err := tx.Conn.ExecContext(ctx, `INSERT INTO channel_watermarks(channel_id,conversation_id,observed_at,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(channel_id,conversation_id) DO UPDATE SET observed_at=max(channel_watermarks.observed_at,excluded.observed_at),updated_at=excluded.updated_at`,
		channelID, conversationID, sentAt, Now())
	return err
}

// CoverageWindow records one attempted history range and its exact outcome.
type CoverageWindow struct {
	ID             string `json:"id"`
	ChannelID      string `json:"channel_id"`
	ConversationID string `json:"conversation_id"`
	StartAt        string `json:"start_at"`
	EndAt          string `json:"end_at"`
	Complete       bool   `json:"complete"`
	Messages       int    `json:"messages"`
	StopReason     string `json:"stop_reason,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
	Gap            string `json:"gap,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// RecordCoverage stores the window result. covered_until advances only when the
// range was fully read and continues an already covered range, so a truncated
// window can never be mistaken for full coverage.
func (tx *Tx) RecordCoverage(ctx context.Context, w CoverageWindow) (Watermark, error) {
	if w.ChannelID == "" || w.ConversationID == "" || w.StartAt == "" || w.EndAt == "" {
		return Watermark{}, Fail("invalid_input", "coverage window requires a channel, conversation and [start,end) range")
	}
	if w.EndAt <= w.StartAt {
		return Watermark{}, Fail("invalid_input", "coverage window must be a non-empty [start,end) range")
	}
	if w.ID == "" {
		w.ID = NewID()
	}
	if !w.Complete && w.StopReason == "" {
		return Watermark{}, Fail("invalid_input", "an incomplete window must carry its stop reason")
	}
	if !w.Complete && w.Gap == "" {
		w.Gap = "range " + w.StartAt + " to " + w.EndAt + " was not fully read: " + w.StopReason
	}
	if _, err := tx.Conn.ExecContext(ctx, "INSERT INTO coverage_windows(id,channel_id,conversation_id,start_at,end_at,complete,messages,stop_reason,cursor,gap,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		w.ID, w.ChannelID, w.ConversationID, w.StartAt, w.EndAt, boolInt(w.Complete), w.Messages, w.StopReason, w.Cursor, w.Gap, Now()); err != nil {
		return Watermark{}, err
	}
	if w.Complete {
		if _, err := tx.Conn.ExecContext(ctx, "UPDATE coverage_windows SET resolved_at=? WHERE channel_id=? AND conversation_id=? AND complete=0 AND resolved_at='' AND julianday(start_at)>=julianday(?) AND julianday(end_at)<=julianday(?)", Now(), w.ChannelID, w.ConversationID, w.StartAt, w.EndAt); err != nil {
			return Watermark{}, err
		}
	}
	current, err := ReadWatermark(ctx, tx.Conn, w.ChannelID, w.ConversationID)
	if err != nil {
		return current, err
	}
	next := current
	switch {
	case !w.Complete:
		next.GapUnresolved = true
	case current.CoveredUntil == "":
		// The first complete window establishes coverage from its own start.
		next.CoveredUntil = w.EndAt
	case w.StartAt <= current.CoveredUntil:
		// The window continues or overlaps the covered range, so coverage extends.
		if w.EndAt > current.CoveredUntil {
			next.CoveredUntil = w.EndAt
		}
	default:
		// A complete window that starts after the covered range leaves the space
		// between them unread. Coverage does not jump over it; the untouched range
		// is recorded as its own unresolved gap.
		next.GapUnresolved = true
		if _, err = tx.Conn.ExecContext(ctx, "INSERT INTO coverage_windows(id,channel_id,conversation_id,start_at,end_at,complete,messages,stop_reason,gap,created_at) VALUES(?,?,?,?,?,0,0,?,?,?)",
			NewID(), w.ChannelID, w.ConversationID, current.CoveredUntil, w.StartAt, "window_started_after_covered_range",
			"range "+current.CoveredUntil+" to "+w.StartAt+" was never read", Now()); err != nil {
			return current, err
		}
	}
	if w.Complete {
		var unresolved int
		if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM coverage_windows WHERE channel_id=? AND conversation_id=? AND complete=0 AND resolved_at=''", w.ChannelID, w.ConversationID).Scan(&unresolved); err != nil {
			return current, err
		}
		next.GapUnresolved = unresolved > 0
	}
	if next.ObservedAt < next.CoveredUntil {
		next.ObservedAt = next.CoveredUntil
	}
	next.UpdatedAt = Now()
	_, err = tx.Conn.ExecContext(ctx, `INSERT INTO channel_watermarks(channel_id,conversation_id,observed_at,covered_until,gap_unresolved,updated_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(channel_id,conversation_id) DO UPDATE SET observed_at=excluded.observed_at,covered_until=excluded.covered_until,gap_unresolved=excluded.gap_unresolved,updated_at=excluded.updated_at`,
		next.ChannelID, next.ConversationID, next.ObservedAt, next.CoveredUntil, boolInt(next.GapUnresolved), next.UpdatedAt)
	return next, err
}

// CoverageReport lists what is covered and what is still missing, so an
// interrupted collection is diagnosable instead of looking finished.
func CoverageReport(ctx context.Context, q Queryer, channelValue string) (any, error) {
	c, err := ReadChannel(ctx, q, channelValue)
	if err != nil {
		return nil, err
	}
	return coverageReport(ctx, q, c, "", "")
}

func DataSourceCoverageReport(ctx context.Context, q Queryer, d DataSource) (any, error) {
	c, err := ReadChannel(ctx, q, d.ChannelID)
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{}
	for _, id := range d.RouteIDs {
		owned[id] = true
	}
	routes := []Route{}
	for _, r := range c.Routes {
		if owned[r.ID] {
			routes = append(routes, r)
		}
	}
	c.Routes = routes
	minimum := time.Now().UTC().AddDate(0, 0, -d.RetentionDays).Format(time.RFC3339Nano)
	return coverageReport(ctx, q, c, minimum, d.DirectEnabledAt)
}

func DataSourceGapCount(ctx context.Context, q Queryer, d DataSource) (int, error) {
	var count int
	minimum := time.Now().UTC().AddDate(0, 0, -d.RetentionDays).Format(time.RFC3339Nano)
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM coverage_windows w JOIN channel_routes r ON r.channel_id=w.channel_id AND r.conversation_id=w.conversation_id WHERE w.channel_id=? AND r.id IN (SELECT value FROM json_each(?)) AND w.complete=0 AND w.resolved_at='' AND julianday(w.end_at)>julianday(?) AND (r.conversation_type<>'direct' OR ?='' OR julianday(w.end_at)>julianday(?))`, d.ChannelID, JSON(d.RouteIDs), minimum, d.DirectEnabledAt, d.DirectEnabledAt).Scan(&count)
	return count, err
}

func coverageReport(ctx context.Context, q Queryer, c Channel, minimum, directSince string) (any, error) {
	conversations := []map[string]any{}
	for _, r := range c.Routes {
		w, err := ReadWatermark(ctx, q, c.ID, r.ConversationID)
		if err != nil {
			return nil, err
		}
		gaps := []map[string]any{}
		routeMinimum := minimum
		if r.ConversationType == "direct" && directSince > routeMinimum {
			routeMinimum = directSince
		}
		rows, err := q.QueryContext(ctx, "SELECT start_at,end_at,stop_reason,gap,cursor FROM coverage_windows WHERE channel_id=? AND conversation_id=? AND complete=0 AND resolved_at='' AND (?='' OR julianday(end_at)>julianday(?)) ORDER BY start_at",
			c.ID, r.ConversationID, routeMinimum, routeMinimum)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var start, end, reason, gap, cursor string
			if err = rows.Scan(&start, &end, &reason, &gap, &cursor); err != nil {
				rows.Close()
				return nil, err
			}
			gaps = append(gaps, map[string]any{"start_at": start, "end_at": end, "stop_reason": reason, "gap": gap, "cursor": cursor})
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, err
		}
		if routeMinimum != "" {
			w.GapUnresolved = len(gaps) > 0
		}
		var complete int
		if err = q.QueryRowContext(ctx, "SELECT count(*) FROM coverage_windows WHERE channel_id=? AND conversation_id=? AND complete=1", c.ID, r.ConversationID).Scan(&complete); err != nil {
			return nil, err
		}
		conversations = append(conversations, map[string]any{"conversation_id": r.ConversationID, "mode": r.Mode,
			"watermark": w, "complete_windows": complete, "gaps": gaps,
			"next_action": nextCoverageAction(w, len(gaps))})
	}
	return map[string]any{"channel": c.Name, "kind": c.Kind, "conversations": conversations, "available_since": minimum,
		"note": "observed_at is what was seen; covered_until only advances over ranges that were read completely"}, nil
}

func nextCoverageAction(w Watermark, gaps int) string {
	switch {
	case w.GapUnresolved || gaps > 0:
		return "re-run channel pull over the listed gap ranges; collection is not complete"
	case w.CoveredUntil == "":
		return "run channel pull to establish an initial covered range"
	default:
		return "collection is continuous up to covered_until; overlap the next window with it"
	}
}

// NextWindow proposes the next backfill range. It deliberately overlaps the
// covered range, because the boundary between dws receiving an event and memgov
// committing it is not transactional and an exact resume cannot be assumed.
func NextWindow(ctx context.Context, q Queryer, channelID, conversationID string, now time.Time, span, overlap time.Duration) (start, end time.Time, err error) {
	if span <= 0 {
		span = time.Hour
	}
	if overlap < 0 {
		overlap = 0
	}
	w, err := ReadWatermark(ctx, q, channelID, conversationID)
	if err != nil {
		return start, end, err
	}
	end = now.UTC()
	if w.CoveredUntil == "" {
		return end.Add(-span), end, nil
	}
	covered, parseErr := time.Parse(time.RFC3339, w.CoveredUntil)
	if parseErr != nil {
		return end.Add(-span), end, nil
	}
	start = covered.Add(-overlap)
	if end.Sub(start) > span {
		end = start.Add(span)
	}
	if !end.After(start) {
		return start, start.Add(span), nil
	}
	return start, end, nil
}
