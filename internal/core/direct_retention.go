package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// Retention is enforced on reads before the asynchronous physical cleanup.
// The predicate only covers routes actually owned by the source.
func retainedSourcePredicate(id string) string {
	return `NOT EXISTS (SELECT 1 FROM source_origins ro JOIN messages rm ON rm.id=ro.message_id
		JOIN data_sources rd ON rd.channel_id=rm.channel_id JOIN channel_routes rr ON rr.channel_id=rm.channel_id AND rr.conversation_id=rm.conversation_id
		WHERE ro.source_id=` + id + ` AND rd.retention_days>0 AND rr.id IN (SELECT value FROM json_each(rd.route_ids))
		AND julianday(rm.sent_at)<julianday('now','-'||rd.retention_days||' days'))`
}

func retainedMessagePredicate(alias string) string {
	return `NOT EXISTS (SELECT 1 FROM data_sources rd JOIN channel_routes rr ON rr.channel_id=rd.channel_id
		WHERE rd.channel_id=` + alias + `.channel_id AND rr.conversation_id=` + alias + `.conversation_id
		AND rr.id IN (SELECT value FROM json_each(rd.route_ids)) AND rd.retention_days>0
		AND julianday(` + alias + `.sent_at)<julianday('now','-'||rd.retention_days||' days'))`
}

func RuntimeContextDataSource(ctx context.Context, q Queryer, c RuntimeConfig) (DataSource, bool, error) {
	d, bound, err := DataSourceForRuntime(ctx, q, c.ID)
	if err != nil || bound {
		return d, bound, err
	}
	channelID := c.ContextChannelID
	if channelID == "" {
		control, readErr := ReadChannel(ctx, q, c.ChannelID)
		if readErr != nil {
			return d, false, readErr
		}
		if control.Kind == ChannelDwsPersonal {
			channelID = control.ID
		} else if control.Identity.HistoryChannel != "" {
			history, readErr := ReadChannel(ctx, q, control.Identity.HistoryChannel)
			if readErr != nil {
				return d, false, readErr
			}
			if history.Tenant != control.Tenant || history.Kind != ChannelDwsPersonal {
				return d, false, Fail("denied", "runtime history source identity mismatch")
			}
			channelID = history.ID
		}
	}
	if channelID == "" {
		return d, false, nil
	}
	d, err = ReadDataSourceByChannel(ctx, q, channelID)
	if ErrorCode(err) == "not_found" {
		return d, false, nil
	}
	return d, err == nil, err
}

func DataSourceRetentionEpoch(ctx context.Context, q Queryer, d DataSource) (string, error) {
	var epoch string
	err := q.QueryRowContext(ctx, `SELECT coalesce(max(av.updated_at),'') FROM source_availability av JOIN source_origins o ON o.source_id=av.source_id WHERE o.channel_id=? AND av.state='expired'`, d.ChannelID).Scan(&epoch)
	if err != nil {
		return "", err
	}
	// Advancing the read boundary also invalidates persistent context before the
	// next hourly physical cleanup, using a stable expiry timestamp.
	var effective string
	err = q.QueryRowContext(ctx, `SELECT coalesce(strftime('%Y-%m-%dT%H:%M:%fZ',max(julianday(m.sent_at)+?)),'') FROM messages m JOIN channel_routes r ON r.channel_id=m.channel_id AND r.conversation_id=m.conversation_id WHERE m.channel_id=? AND r.id IN (SELECT value FROM json_each(?)) AND julianday(m.sent_at)+?<=julianday('now')`, d.RetentionDays, d.ChannelID, JSON(d.RouteIDs), d.RetentionDays).Scan(&effective)
	if err != nil {
		return "", err
	}
	physicalTime, _ := time.Parse(time.RFC3339Nano, epoch)
	effectiveTime, _ := time.Parse(time.RFC3339Nano, effective)
	if effectiveTime.After(physicalTime) {
		epoch = effective
	}
	return epoch, nil
}

func sourceRawExpired(ctx context.Context, q Queryer, id string) (bool, error) {
	var expired bool
	err := q.QueryRowContext(ctx, `SELECT NOT (`+retainedSourcePredicate("?")+`) OR EXISTS(SELECT 1 FROM source_availability WHERE source_id=? AND state='expired')`, id, id).Scan(&expired)
	return expired, err
}

func redactCachedRetentionResult(ctx context.Context, q Queryer, raw string) (string, error) {
	return redactCachedRetentionCopy(ctx, q, raw, nil)
}

func redactCachedRetentionCopy(ctx context.Context, q Queryer, raw string, texts []string) (string, error) {
	if !strings.Contains(raw, `"content"`) && !strings.Contains(raw, `"body"`) && !strings.Contains(raw, `"quote"`) && !strings.Contains(raw, `"result"`) {
		return raw, nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", err
	}
	var err error
	if texts == nil {
		rows, err := q.QueryContext(ctx, `SELECT mr.body,mr.snapshot FROM messages m JOIN message_revisions mr ON mr.message_id=m.id WHERE (m.availability='expired' OR NOT (`+retainedMessagePredicate("m")+`))`)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var body, snapshot string
			if err = rows.Scan(&body, &snapshot); err != nil {
				rows.Close()
				return "", err
			}
			for _, text := range messageRawTexts(body, snapshot) {
				if text == "" {
					continue
				}
				texts = append(texts, text)
				for _, line := range strings.Split(text, "\n") {
					if strings.TrimSpace(line) != "" {
						texts = append(texts, line)
					}
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
	}
	// Replace complete (including JSON-escaped) quotes before their individual
	// lines; otherwise a prefix replacement can conceal the remaining raw tail.
	texts = append([]string(nil), texts...)
	sort.Slice(texts, func(i, j int) bool { return len(texts[i]) > len(texts[j]) })
	var walk func(any, bool, bool) (any, error)
	walk = func(node any, expired, textField bool) (any, error) {
		switch item := node.(type) {
		case map[string]any:
			source, _ := item["source_id"].(string)
			if source == "" {
				if _, ok := item["fragments"]; ok {
					source, _ = item["id"].(string)
				}
			}
			if source != "" {
				gone, e := sourceRawExpired(ctx, q, source)
				if e != nil {
					return nil, e
				}
				expired = expired || gone
			}
			for key, child := range item {
				if expired && contains([]string{"body", "content", "quote"}, key) {
					item[key] = ""
					continue
				}
				next, e := walk(child, expired, contains([]string{"body", "content", "quote", "title", "result", "instructions", "result_summary", "summary", "output", "payload"}, key))
				if e != nil {
					return nil, e
				}
				item[key] = next
			}
		case []any:
			for i, child := range item {
				next, e := walk(child, expired, textField)
				if e != nil {
					return nil, e
				}
				item[i] = next
			}
		case string:
			if textField {
				for _, text := range texts {
					item = strings.ReplaceAll(item, text, "\x00memgov-expired\x00")
				}
				return strings.ReplaceAll(item, "\x00memgov-expired\x00", "[原文已到期，可回查]"), nil
			}
		}
		return node, nil
	}
	value, err = walk(value, false, false)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

type DirectContact struct {
	ChannelID      string `json:"channel_id"`
	ConversationID string `json:"conversation_id"`
	PeerIDType     string `json:"peer_id_type,omitempty"`
	PeerIDValue    string `json:"peer_id_value,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	ObservedAt     string `json:"observed_at"`
}

// EnsureDataSourceDirectConversation admits only a real provider conversation
// observed after the explicit enable boundary. It never reuses the owner's
// application-bot delivery address.
func (tx *Tx) EnsureDataSourceDirectConversation(ctx context.Context, sourceValue string, e NormalizedEvent) (Route, error) {
	d, err := ReadDataSource(ctx, tx.Conn, sourceValue)
	if err != nil {
		return Route{}, err
	}
	if !d.Enabled || !d.DirectEnabled || d.Status != "running" || e.ConversationType != "direct" {
		return Route{}, Fail("denied", "direct collection is not enabled for this source")
	}
	if e.Sender.IDType == "robot_code" {
		return Route{}, Fail("denied", "robot direct messages are excluded from colleague collection")
	}
	sent, parseErr := time.Parse(time.RFC3339, e.SentAt)
	enabled, enableErr := time.Parse(time.RFC3339, d.DirectEnabledAt)
	if parseErr != nil || enableErr != nil || sent.Before(enabled) {
		return Route{}, Fail("denied", "direct message predates the current collection enable boundary")
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -d.RetentionDays)
	if d.RetentionDays > 0 && sent.Before(cutoff) {
		return Route{}, Fail("denied", "direct message is outside the retention window")
	}
	c, err := ReadChannel(ctx, tx.Conn, d.ChannelID)
	if err != nil {
		return Route{}, err
	}
	peerType, peerValue, peerName := e.Sender.IDType, e.Sender.IDValue, e.Sender.DisplayName
	if e.Sender.SelfAuthor || peerValue == c.Identity.ExpectedUserID {
		peerType, peerValue, peerName = "", "", ""
	}
	r, err := tx.AdmitDataSourceDirectConversation(ctx, d.ID, e.ConversationID, peerName)
	if err != nil {
		return Route{}, err
	}
	observed := e.EventAt
	if observed == "" {
		observed = e.SentAt
	}
	_, err = tx.Conn.ExecContext(ctx, `INSERT INTO direct_conversation_contacts(channel_id,conversation_id,peer_id_type,peer_id_value,display_name,observed_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(channel_id,conversation_id) DO UPDATE SET
		peer_id_type=CASE WHEN excluded.peer_id_value<>'' THEN excluded.peer_id_type ELSE peer_id_type END,
		peer_id_value=CASE WHEN excluded.peer_id_value<>'' THEN excluded.peer_id_value ELSE peer_id_value END,
		display_name=CASE WHEN excluded.display_name<>'' THEN excluded.display_name ELSE display_name END,
		observed_at=excluded.observed_at`, d.ChannelID, e.ConversationID, peerType, peerValue, peerName, observed)
	if err == nil {
		_, err = tx.Conn.ExecContext(ctx, "UPDATE data_sources SET last_direct_received_at=?,updated_at=? WHERE id=?", Now(), Now(), d.ID)
	}
	return r, err
}

// AdmitDataSourceDirectConversation adds a provider conversation discovered by
// a bounded direct-message search. It does not write a message or infer a peer
// identity from the display name.
func (tx *Tx) AdmitDataSourceDirectConversation(ctx context.Context, sourceValue, conversation, displayName string) (Route, error) {
	d, err := ReadDataSource(ctx, tx.Conn, sourceValue)
	if err != nil {
		return Route{}, err
	}
	if !d.Enabled || !d.DirectEnabled || d.Status != "running" || conversation == "" {
		return Route{}, Fail("denied", "direct collection is not enabled for this source")
	}
	r, err := RouteFor(ctx, tx.Conn, d.ChannelID, conversation)
	if ErrorCode(err) == "denied" {
		r, err = tx.AddRoute(ctx, d.ChannelID, RouteInput{ConversationID: conversation, ConversationType: "direct", Workspace: d.WorkspaceID, Mode: "collect", AudiencePolicy: "local_private", SendPolicy: "draft_only"})
	}
	if err != nil {
		return Route{}, err
	}
	if r.ConversationType != "direct" || r.Mode == "ignore" || r.Status != "active" {
		return Route{}, Fail("denied", "conversation is not an active direct collection route")
	}
	found := false
	for _, id := range d.RouteIDs {
		if id == r.ID {
			found = true
			break
		}
	}
	if !found {
		d.RouteIDs = append(d.RouteIDs, r.ID)
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE data_sources SET route_ids=?,updated_at=? WHERE id=?", JSON(d.RouteIDs), Now(), d.ID); err != nil {
			return Route{}, err
		}
	}
	_, err = tx.Conn.ExecContext(ctx, `INSERT INTO direct_conversation_contacts(channel_id,conversation_id,peer_id_type,peer_id_value,display_name,observed_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(channel_id,conversation_id) DO UPDATE SET
		display_name=CASE WHEN excluded.display_name<>'' THEN excluded.display_name ELSE display_name END,
		observed_at=excluded.observed_at`, d.ChannelID, conversation, "", "", displayName, Now())
	return r, err
}

type RetentionResult struct {
	Source    string `json:"source"`
	Days      int    `json:"days"`
	Cutoff    string `json:"cutoff"`
	Expired   int    `json:"expired"`
	LastRunAt string `json:"last_run_at"`
	ErrorCode string `json:"error_code,omitempty"`
}

type RetentionPreview struct {
	Source   string `json:"source"`
	Days     int    `json:"days"`
	Eligible int    `json:"eligible_raw_messages"`
}

// PreviewDataSourceRetention reports how many currently available message
// bodies would expire under the requested policy. It is read-only so config
// planning can expose the first cleanup before the policy is applied.
func PreviewDataSourceRetention(ctx context.Context, q Queryer, sourceValue string, days int, now time.Time) (RetentionPreview, error) {
	d, err := ReadDataSource(ctx, q, sourceValue)
	if err != nil {
		return RetentionPreview{}, err
	}
	out := RetentionPreview{Source: d.Name, Days: days}
	if days <= 0 || len(d.RouteIDs) == 0 {
		return out, nil
	}
	cutoff := now.UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN channel_routes r ON r.channel_id=m.channel_id AND r.conversation_id=m.conversation_id
		WHERE m.channel_id=? AND m.availability='available' AND julianday(m.sent_at)<julianday(?)
		AND r.id IN (SELECT value FROM json_each(?))`, d.ChannelID, cutoff, JSON(d.RouteIDs)).Scan(&out.Eligible)
	return out, err
}

// ApplyDataSourceRetention makes raw text unavailable while preserving message,
// source locator, hashes, dedupe rows, task state and audit metadata.
func (tx *Tx) ApplyDataSourceRetention(ctx context.Context, sourceValue string, now time.Time, limit int) (RetentionResult, error) {
	d, err := ReadDataSource(ctx, tx.Conn, sourceValue)
	if err != nil {
		return RetentionResult{}, err
	}
	out := RetentionResult{Source: d.Name, Days: d.RetentionDays}
	if d.RetentionDays <= 0 {
		return out, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	cutoff := now.UTC().AddDate(0, 0, -d.RetentionDays)
	out.Cutoff = cutoff.Format(time.RFC3339Nano)
	rows, err := tx.Conn.QueryContext(ctx, `SELECT m.id FROM messages m JOIN channel_routes r ON r.channel_id=m.channel_id AND r.conversation_id=m.conversation_id
		WHERE m.channel_id=? AND m.availability='available' AND julianday(m.sent_at)<julianday(?)
		AND r.id IN (SELECT value FROM json_each(?)) ORDER BY m.sent_at,m.id LIMIT ?`, d.ChannelID, out.Cutoff, JSON(d.RouteIDs), limit)
	if err != nil {
		return out, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	expiredSources := map[string]bool{}
	rawOriginals := map[string]bool{}
	for _, id := range ids {
		bodyRows, bodyErr := tx.Conn.QueryContext(ctx, "SELECT body,snapshot FROM message_revisions WHERE message_id=?", id)
		if bodyErr != nil {
			return out, bodyErr
		}
		for bodyRows.Next() {
			var body, snapshot string
			if bodyErr = bodyRows.Scan(&body, &snapshot); bodyErr != nil {
				bodyRows.Close()
				return out, bodyErr
			}
			for _, text := range messageRawTexts(body, snapshot) {
				if text == "" {
					continue
				}
				rawOriginals[text] = true
				for _, line := range strings.Split(text, "\n") {
					if strings.TrimSpace(line) != "" {
						rawOriginals[line] = true
					}
				}
			}
		}
		bodyErr = bodyRows.Err()
		bodyRows.Close()
		if bodyErr != nil {
			return out, bodyErr
		}
		sourceRows, sourceErr := tx.Conn.QueryContext(ctx, "SELECT source_id FROM source_origins WHERE message_id=?", id)
		if sourceErr != nil {
			return out, sourceErr
		}
		sourceIDs := []string{}
		for sourceRows.Next() {
			var sourceID string
			if sourceErr = sourceRows.Scan(&sourceID); sourceErr != nil {
				sourceRows.Close()
				return out, sourceErr
			}
			sourceIDs = append(sourceIDs, sourceID)
		}
		sourceErr = sourceRows.Err()
		sourceRows.Close()
		if sourceErr != nil {
			return out, sourceErr
		}
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE message_revisions SET body='',snapshot='{}' WHERE message_id=?", id); err != nil {
			return out, err
		}
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE message_attachments SET name='',media_type='',resource_id='',state='expired' WHERE message_id=?", id); err != nil {
			return out, err
		}
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE inbox_events SET payload='{}' WHERE message_id=?", id); err != nil {
			return out, err
		}
		if _, err = tx.Conn.ExecContext(ctx, "UPDATE messages SET availability='expired',availability_reason='raw text expired by retention policy',updated_at=? WHERE id=?", Now(), id); err != nil {
			return out, err
		}
		for _, sourceID := range sourceIDs {
			expiredSources[sourceID] = true
			if _, err = tx.Conn.ExecContext(ctx, "UPDATE sources SET content='',redacted=1 WHERE id=?", sourceID); err != nil {
				return out, err
			}
			if _, err = tx.Conn.ExecContext(ctx, "UPDATE fragments SET content='' WHERE source_id=?", sourceID); err != nil {
				return out, err
			}
			if _, err = tx.Conn.ExecContext(ctx, `INSERT INTO source_availability(source_id,state,reason,change_seq,updated_at) VALUES(?,'expired','raw text expired; re-query platform to verify',1,?)
				ON CONFLICT(source_id) DO UPDATE SET state='expired',reason=excluded.reason,change_seq=source_availability.change_seq+1,updated_at=excluded.updated_at`, sourceID, Now()); err != nil {
				return out, err
			}
			if _, err = tx.Conn.ExecContext(ctx, "DELETE FROM search_fts WHERE kind='source' AND ref_id=?", sourceID); err != nil {
				return out, err
			}
		}
		// Task identity and state remain. Stored model transcripts/results
		// are cleared because they may contain verbatim copies of the expired text.
		for _, statement := range []string{
			"UPDATE runtime_batches SET output='' WHERE id IN (SELECT batch_id FROM runtime_batch_messages WHERE message_id=?)",
			"UPDATE runtime_attempts SET output='' WHERE task_id IN (SELECT task_id FROM runtime_task_messages WHERE message_id=?)",
			"UPDATE runtime_tasks SET instructions='',result='' WHERE id IN (SELECT task_id FROM runtime_task_messages WHERE message_id=?)",
			"UPDATE runtime_pending_actions SET payload='',payload_digest='',status=CASE WHEN status IN ('pending','confirmed') THEN 'stale' WHEN status='executing' THEN 'unknown' ELSE status END WHERE task_id IN (SELECT task_id FROM runtime_task_messages WHERE message_id=?)",
			"UPDATE runtime_action_attempts SET result='' WHERE task_id IN (SELECT task_id FROM runtime_task_messages WHERE message_id=?)",
		} {
			if _, err = tx.Conn.ExecContext(ctx, statement, id); err != nil {
				return out, err
			}
		}
		if _, err = tx.Conn.ExecContext(ctx, `UPDATE runtime_message_actions SET content='',reason='',receipt='',detail='source evidence unavailable'
WHERE task_id IN (SELECT task_id FROM runtime_task_messages WHERE message_id=?)
OR EXISTS(SELECT 1 FROM json_each(runtime_message_actions.evidence_message_ids) WHERE value=?)`, id, id); err != nil {
			return out, err
		}
	}
	// Include earlier batches so upgraded cleanup also repairs historical quotes
	// and their digests from prior runs.
	expiredRows, expiredErr := tx.Conn.QueryContext(ctx, "SELECT source_id FROM source_availability WHERE state='expired'")
	if expiredErr != nil {
		return out, expiredErr
	}
	for expiredRows.Next() {
		var sid string
		if expiredErr = expiredRows.Scan(&sid); expiredErr != nil {
			expiredRows.Close()
			return out, expiredErr
		}
		expiredSources[sid] = true
	}
	expiredErr = expiredRows.Err()
	expiredRows.Close()
	if expiredErr != nil {
		return out, expiredErr
	}
	if err = tx.redactRuntimeConclusions(ctx, rawOriginals); err != nil {
		return out, err
	}
	out.Expired = len(ids)
	out.LastRunAt = now.UTC().Format(time.RFC3339Nano)
	_, err = tx.Conn.ExecContext(ctx, "UPDATE data_sources SET last_retention_at=?,last_retention_count=?,retention_error_code='',updated_at=? WHERE id=?", out.LastRunAt, out.Expired, Now(), d.ID)
	return out, err
}

// Retain semantic execution conclusions, stripping known source originals and
// evidence excerpts from summaries and replies, including JSON-escaped copies.
func (tx *Tx) redactRuntimeConclusions(ctx context.Context, originals map[string]bool) error {
	if len(originals) == 0 {
		return nil
	}
	texts := []string{}
	for text := range originals {
		texts = append(texts, text)
		encoded, _ := json.Marshal(text)
		if len(encoded) > 2 {
			texts = append(texts, string(encoded[1:len(encoded)-1]))
		}
	}
	sort.Slice(texts, func(i, j int) bool { return len(texts[i]) > len(texts[j]) })
	for _, target := range []struct{ table, field string }{{"runtime_tasks", "result_summary"}, {"runtime_tasks", "result"}, {"runtime_attempts", "summary"}, {"runtime_action_attempts", "summary"}, {"outbox", "content"}, {"idempotency", "result"}} {
		identity := "id"
		filter := target.field + "<>''"
		if target.table == "idempotency" {
			identity = "rowid"
			// Legacy file-migration snapshots are outside managed chat sources.
			// Scan them only when a migration actually references expired chat
			// sources; otherwise tens of MB of unrelated snapshots delay renewal.
			filter += ` AND (instr(result,'"content"')>0 OR instr(result,'"body"')>0 OR instr(result,'"quote"')>0 OR instr(result,'"result"')>0)`
		}
		rows, err := tx.Conn.QueryContext(ctx, "SELECT "+identity+","+target.field+" FROM "+target.table+" WHERE "+filter)
		if err != nil {
			return err
		}
		type update struct{ id, text string }
		updates := []update{}
		for rows.Next() {
			var id, body string
			if err = rows.Scan(&id, &body); err != nil {
				rows.Close()
				return err
			}
			redacted := body
			if target.table == "idempotency" {
				redacted, err = redactCachedRetentionCopy(ctx, tx.Conn, body, texts)
				if err != nil {
					rows.Close()
					return err
				}
			} else {
				for _, text := range texts {
					redacted = strings.ReplaceAll(redacted, text, "\x00memgov-expired\x00")
				}
			}
			redacted = strings.ReplaceAll(redacted, "\x00memgov-expired\x00", "[原文已到期，可回查]")
			if redacted != body {
				updates = append(updates, update{id, redacted})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, u := range updates {
			if target.table == "idempotency" && !json.Valid([]byte(u.text)) {
				u.text = "null"
			}
			if _, err = tx.Conn.ExecContext(ctx, "UPDATE "+target.table+" SET "+target.field+"=? WHERE "+identity+"=?", u.text, u.id); err != nil {
				return err
			}
		}
	}
	// Raw model transcripts are disposable and may contain unstructured copies
	// whose excerpt boundaries cannot be recovered reliably.
	for _, table := range []string{"runtime_attempts", "runtime_batches"} {
		if _, err := tx.Conn.ExecContext(ctx, "UPDATE "+table+" SET output='' WHERE output<>''"); err != nil {
			return err
		}
	}
	return nil
}

func redactReadRuntimeTask(ctx context.Context, q Queryer, t *RuntimeTask) error {
	rows, err := q.QueryContext(ctx, `SELECT mr.body,mr.snapshot FROM runtime_task_messages tm JOIN messages m ON m.id=tm.message_id JOIN message_revisions mr ON mr.message_id=m.id WHERE tm.task_id=? AND (m.availability='expired' OR NOT (`+retainedMessagePredicate("m")+`))`, t.ID)
	if err != nil {
		return err
	}
	texts := []string{}
	expired := false
	for rows.Next() {
		var body, snapshot string
		if err = rows.Scan(&body, &snapshot); err != nil {
			rows.Close()
			return err
		}
		expired = true
		for _, text := range messageRawTexts(body, snapshot) {
			if text == "" {
				continue
			}
			texts = append(texts, text)
			for _, line := range strings.Split(text, "\n") {
				if strings.TrimSpace(line) != "" {
					texts = append(texts, line)
				}
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if expired {
		t.Instructions = ""
		t.Result = ""
		for _, text := range texts {
			t.ResultSummary = strings.ReplaceAll(t.ResultSummary, text, "\x00memgov-expired\x00")
		}
		t.ResultSummary = strings.ReplaceAll(t.ResultSummary, "\x00memgov-expired\x00", "[原文已到期，可回查]")
		for i := range t.Attempts {
			for _, text := range texts {
				t.Attempts[i].Summary = strings.ReplaceAll(t.Attempts[i].Summary, text, "[原文已到期，可回查]")
			}
		}
		for i := range t.Actions {
			t.Actions[i].Payload = ""
			for j := range t.Actions[i].Attempts {
				t.Actions[i].Attempts[j].Result = ""
				for _, text := range texts {
					t.Actions[i].Attempts[j].Summary = strings.ReplaceAll(t.Actions[i].Attempts[j].Summary, text, "[原文已到期，可回查]")
				}
			}
		}
	}
	return nil
}

func ReadDirectContact(ctx context.Context, q Queryer, channelID, conversation string) (DirectContact, error) {
	var c DirectContact
	err := q.QueryRowContext(ctx, "SELECT channel_id,conversation_id,peer_id_type,peer_id_value,display_name,observed_at FROM direct_conversation_contacts WHERE channel_id=? AND conversation_id=?", channelID, conversation).Scan(&c.ChannelID, &c.ConversationID, &c.PeerIDType, &c.PeerIDValue, &c.DisplayName, &c.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, Fail("not_found", "direct contact not found")
	}
	return c, err
}
