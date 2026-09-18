package core

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// HistoryImport owns a fixed range. The adapter-owned continuation cursor never changes
// its bounds, and completed imports are retained independently of log retention.
type HistoryImport struct {
	ID            string `json:"id"`
	DataSourceID  string `json:"data_source_id"`
	ChannelID     string `json:"channel_id"`
	RouteID       string `json:"route_id"`
	StartAt       string `json:"start_at"`
	EndAt         string `json:"end_at"`
	Cursor        string `json:"cursor,omitempty"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	Failures      int    `json:"failures"`
	Events        int    `json:"events"`
	Applied       int    `json:"applied"`
	Duplicates    int    `json:"duplicates"`
	LeaseToken    string `json:"-"`
	LeaseUntil    string `json:"lease_until,omitempty"`
	NextAttemptAt string `json:"next_attempt_at,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	StopReason    string `json:"stop_reason,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

const historyImportColumns = "id,data_source_id,channel_id,route_id,start_at,end_at,cursor,status,attempts,failures,events,applied,duplicates,lease_token,lease_until,next_attempt_at,error_code,stop_reason,created_at,updated_at"

func scanHistoryImport(row interface{ Scan(...any) error }) (HistoryImport, error) {
	var h HistoryImport
	err := row.Scan(&h.ID, &h.DataSourceID, &h.ChannelID, &h.RouteID, &h.StartAt, &h.EndAt, &h.Cursor, &h.Status, &h.Attempts, &h.Failures, &h.Events, &h.Applied, &h.Duplicates, &h.LeaseToken, &h.LeaseUntil, &h.NextAttemptAt, &h.ErrorCode, &h.StopReason, &h.CreatedAt, &h.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return h, Fail("not_found", "history import not found")
	}
	return h, err
}
func ReadHistoryImport(ctx context.Context, q Queryer, id string) (HistoryImport, error) {
	return scanHistoryImport(q.QueryRowContext(ctx, "SELECT "+historyImportColumns+" FROM history_imports WHERE id=?", id))
}
func ListHistoryImports(ctx context.Context, q Queryer, sourceID string) ([]HistoryImport, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+historyImportColumns+" FROM history_imports WHERE data_source_id=? ORDER BY created_at,id", sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryImport{}
	for rows.Next() {
		h, e := scanHistoryImport(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (tx *Tx) CreateHistoryImport(ctx context.Context, sourceValue, routeID string, start, end time.Time) (HistoryImport, error) {
	source, err := ReadDataSource(ctx, tx.Conn, sourceValue)
	if err != nil {
		return HistoryImport{}, err
	}
	route, err := ReadRoute(ctx, tx.Conn, routeID)
	if err != nil {
		return HistoryImport{}, err
	}
	if route.ConversationType != "group" || route.ChannelID != source.ChannelID || route.Mode == "ignore" || route.Status != "active" || !contains(source.RouteIDs, route.ID) {
		return HistoryImport{}, Fail("forbidden", "history route is outside the active data source")
	}
	if !end.After(start) || end.Sub(start) > 30*24*time.Hour {
		return HistoryImport{}, Fail("invalid_input", "history import requires a fixed range of at most 30 days")
	}
	startAt, endAt := start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)
	if h, e := scanHistoryImport(tx.Conn.QueryRowContext(ctx, "SELECT "+historyImportColumns+" FROM history_imports WHERE data_source_id=? AND route_id=? AND start_at=? AND end_at=?", source.ID, routeID, startAt, endAt)); e == nil {
		return h, nil
	} else if ErrorCode(e) != "not_found" {
		return HistoryImport{}, e
	}
	id, now := NewID(), Now()
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO history_imports(id,data_source_id,channel_id,route_id,start_at,end_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", id, source.ID, source.ChannelID, routeID, startAt, endAt, now, now)
	if err != nil {
		return HistoryImport{}, err
	}
	return ReadHistoryImport(ctx, tx.Conn, id)
}

// EnsureHistoryImports schedules one initial 30-day import per admitted route.
// A cancelled import remains cancelled; only an explicit retry resumes it.
func (tx *Tx) EnsureHistoryImports(ctx context.Context, sourceID string, now time.Time) error {
	source, err := ReadDataSource(ctx, tx.Conn, sourceID)
	if err != nil {
		return err
	}
	if !source.HistoryEnabled {
		return nil
	}
	for _, id := range source.RouteIDs {
		r, e := ReadRoute(ctx, tx.Conn, id)
		if e != nil {
			return e
		}
		if r.ConversationType != "group" || r.Mode == "ignore" || r.Status != "active" {
			continue
		}
		var count int
		if e = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM history_imports WHERE data_source_id=? AND route_id=?", source.ID, id).Scan(&count); e != nil {
			return e
		}
		if count == 0 {
			if _, e = tx.CreateHistoryImport(ctx, source.ID, id, now.Add(-time.Duration(source.HistoryDays)*24*time.Hour), now); e != nil {
				return e
			}
		}
	}
	return nil
}
func (tx *Tx) CancelHistoryImport(ctx context.Context, id string) (HistoryImport, error) {
	h, err := ReadHistoryImport(ctx, tx.Conn, id)
	if err != nil {
		return h, err
	}
	if h.Status == "completed" {
		return h, Fail("conflict", "completed history import cannot be cancelled")
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE history_imports SET status='cancelled',lease_token='',lease_until='',updated_at=? WHERE id=?", Now(), id)
	if err != nil {
		return h, err
	}
	return ReadHistoryImport(ctx, tx.Conn, id)
}
func (tx *Tx) RetryHistoryImport(ctx context.Context, id string) (HistoryImport, error) {
	h, err := ReadHistoryImport(ctx, tx.Conn, id)
	if err != nil {
		return h, err
	}
	if h.Status != "failed" && h.Status != "cancelled" {
		return h, Fail("conflict", "only failed or cancelled imports can be retried")
	}
	cursor, err := tx.recoverLegacyHistoryCursor(ctx, h)
	if err != nil {
		return h, err
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE history_imports SET status='queued',cursor=?,failures=0,error_code='',next_attempt_at='',lease_token='',lease_until='',updated_at=? WHERE id=?", cursor, Now(), id)
	if err != nil {
		return h, err
	}
	return ReadHistoryImport(ctx, tx.Conn, id)
}

// ClaimHistoryImport serializes imports per data source even when two workers
// race. An expired claim is recoverable; stale workers cannot commit its result.
func (tx *Tx) ClaimHistoryImport(ctx context.Context, sourceID string, now time.Time) (HistoryImport, error) {
	source, err := ReadDataSource(ctx, tx.Conn, sourceID)
	if err != nil {
		return HistoryImport{}, err
	}
	if source.Status != "running" {
		return HistoryImport{}, Fail("not_found", "data source is not running")
	}
	at := now.UTC().Format(time.RFC3339)
	var n int
	if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM history_imports WHERE data_source_id=? AND status='running' AND lease_until>?", source.ID, at).Scan(&n); err != nil {
		return HistoryImport{}, err
	}
	if n > 0 {
		return HistoryImport{}, Fail("not_found", "history worker already has an active step")
	}
	h, err := scanHistoryImport(tx.Conn.QueryRowContext(ctx, "SELECT "+historyImportColumns+" FROM history_imports WHERE data_source_id=? AND route_id IN (SELECT value FROM json_each(?)) AND route_id IN (SELECT id FROM channel_routes WHERE status='active' AND mode!='ignore') AND ((status='queued' AND next_attempt_at<=?) OR (status='running' AND lease_until<=?)) ORDER BY updated_at,id LIMIT 1", source.ID, JSON(source.RouteIDs), at, at))
	if err != nil {
		return h, err
	}
	route, err := ReadRoute(ctx, tx.Conn, h.RouteID)
	if err != nil {
		return h, err
	}
	if route.Mode == "ignore" || route.Status != "active" || !contains(source.RouteIDs, h.RouteID) {
		return HistoryImport{}, Fail("forbidden", "history route is no longer active")
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE history_imports SET status='running',attempts=attempts+1,lease_token=?,lease_until=?,updated_at=? WHERE id=?", NewID(), now.Add(2*time.Minute).UTC().Format(time.RFC3339), at, h.ID)
	if err != nil {
		return h, err
	}
	return ReadHistoryImport(ctx, tx.Conn, h.ID)
}
func (tx *Tx) checkHistoryClaim(ctx context.Context, h HistoryImport) (HistoryImport, error) {
	current, err := ReadHistoryImport(ctx, tx.Conn, h.ID)
	if err != nil {
		return current, err
	}
	if current.Status != "running" || current.LeaseToken == "" || current.LeaseToken != h.LeaseToken || current.LeaseUntil <= Now() {
		return current, Fail("conflict", "history import claim expired or was cancelled")
	}
	source, err := ReadDataSource(ctx, tx.Conn, current.DataSourceID)
	if err != nil {
		return current, err
	}
	r, err := ReadRoute(ctx, tx.Conn, current.RouteID)
	if err != nil {
		return current, err
	}
	if source.Status != "running" || !contains(source.RouteIDs, r.ID) || r.Mode == "ignore" || r.Status != "active" {
		return current, Fail("forbidden", "history route is no longer authorized")
	}
	return current, nil
}

// FinishHistoryImport commits events, explicit context provenance, coverage and
// the continuation cursor together. An intake failure rolls the entire step back.
func (tx *Tx) FinishHistoryImport(ctx context.Context, claim HistoryImport, events []NormalizedEvent, complete bool, cursor, reason string) (HistoryImport, error) {
	h, err := tx.checkHistoryClaim(ctx, claim)
	if err != nil {
		return h, err
	}
	r, err := ReadRoute(ctx, tx.Conn, h.RouteID)
	if err != nil {
		return h, err
	}
	if len(events) > 200 {
		return h, Fail("invalid_input", "history step exceeded item limit")
	}
	stalled := !complete && (cursor == "" || cursor == h.Cursor)
	if stalled {
		// Preserve the bounded page and its unresolved coverage atomically, but
		// never claim a checkpoint advance or schedule an endless replay loop.
		cursor = h.Cursor
		if reason == "" {
			reason = "cursor_not_advancing"
		}
	}
	if complete && cursor != "" {
		return h, Fail("invalid_input", "complete history step has a continuation cursor")
	}
	applied, duplicates := 0, 0
	for _, event := range events {
		if event.ConversationID != r.ConversationID {
			return h, Fail("forbidden", "history event belongs to another conversation")
		}
		eventTime, timeErr := time.Parse(time.RFC3339, event.SentAt)
		stamp := eventTime.UTC().Format(time.RFC3339)
		if event.Kind != EventRecall && (timeErr != nil || stamp < h.StartAt || stamp >= h.EndAt) {
			return h, Fail("invalid_input", "history event lies outside fixed import range")
		}
		// Preserve existing live-message eligibility. A new imported identity, even
		// one edited later by the live stream, never becomes a fresh task trigger.
		var existing int
		if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE channel_id=? AND conversation_id=? AND provider_message_id=?", h.ChannelID, r.ConversationID, event.ProviderMessageID).Scan(&existing); err != nil {
			return h, err
		}
		event.Origin = "history"
		result, e := tx.Intake(ctx, h.ChannelID, event)
		if e != nil {
			return h, e
		}
		if existing == 0 && result.MessageID != "" {
			if _, e = tx.Conn.ExecContext(ctx, "UPDATE messages SET context_only=1 WHERE id=?", result.MessageID); e != nil {
				return h, e
			}
		}
		if result.Duplicate {
			duplicates++
		} else {
			applied++
		}
		if event.Kind != EventRecall && result.Status != "ignored" {
			if e = tx.ObserveMessage(ctx, h.ChannelID, r.ConversationID, event.SentAt); e != nil {
				return h, e
			}
		}
	}
	if !complete && reason == "" {
		reason = "bounded_step"
	}
	if _, err = tx.RecordCoverage(ctx, CoverageWindow{ChannelID: h.ChannelID, ConversationID: r.ConversationID, StartAt: h.StartAt, EndAt: h.EndAt, Complete: complete, Messages: len(events), StopReason: reason, Cursor: cursor}); err != nil {
		return h, err
	}
	if complete {
		// Earlier partial attempts for this exact window are now resolved. Keep their
		// evidence rows, but do not continue reporting a gap already fully traversed.
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE coverage_windows SET complete=1,gap='',stop_reason='resolved_by_import',cursor='' WHERE channel_id=? AND conversation_id=? AND start_at=? AND end_at=? AND complete=0", h.ChannelID, r.ConversationID, h.StartAt, h.EndAt); err != nil {
			return h, err
		}
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE channel_watermarks SET gap_unresolved=EXISTS(SELECT 1 FROM coverage_windows WHERE channel_id=? AND conversation_id=? AND complete=0) WHERE channel_id=? AND conversation_id=?", h.ChannelID, r.ConversationID, h.ChannelID, r.ConversationID); err != nil {
			return h, err
		}
	}
	status, errorCode := "queued", ""
	if complete {
		status = "completed"
	} else if stalled {
		status, errorCode = "failed", "history_cursor_stalled"
	}
	_, err = tx.Conn.ExecContext(ctx, "UPDATE history_imports SET status=?,cursor=?,events=events+?,applied=applied+?,duplicates=duplicates+?,failures=0,error_code=?,stop_reason=?,lease_token='',lease_until='',next_attempt_at='',updated_at=? WHERE id=?", status, cursor, len(events), applied, duplicates, errorCode, reason, Now(), h.ID)
	if err != nil {
		return h, err
	}
	return ReadHistoryImport(ctx, tx.Conn, h.ID)
}

// FailHistoryImport retains the old cursor and records only a bounded code.
func (tx *Tx) FailHistoryImport(ctx context.Context, claim HistoryImport, code string) (HistoryImport, error) {
	h, err := tx.checkHistoryClaim(ctx, claim)
	if err != nil {
		return h, err
	}
	switch code {
	case "rate_limited", "unavailable", "timeout", "invalid_input", "forbidden", "denied", "conflict":
	default:
		code = "history_step_failed"
	}
	status := "queued"
	if h.Failures >= 4 || code == "invalid_input" || code == "forbidden" || code == "denied" {
		status = "failed"
	}
	delay := 30 * time.Second * time.Duration(1<<min(h.Failures, 4))
	_, err = tx.Conn.ExecContext(ctx, "UPDATE history_imports SET status=?,failures=failures+1,error_code=?,stop_reason='step_failed',lease_token='',lease_until='',next_attempt_at=?,updated_at=? WHERE id=?", status, code, time.Now().Add(delay).UTC().Format(time.RFC3339), Now(), h.ID)
	if err != nil {
		return h, err
	}
	return ReadHistoryImport(ctx, tx.Conn, h.ID)
}
