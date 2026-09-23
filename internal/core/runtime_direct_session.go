package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// DirectCommand recognizes complete registered protocol commands only.
// All natural language, including cancellation and follow-ups, belongs to Agent.
func DirectCommand(body string) string {
	switch strings.TrimSpace(body) {
	case "/clear":
		return "clear"
	case "/status":
		return "status"
	default:
		return ""
	}
}

type RuntimeDirectSession struct {
	ID                  string `json:"id"`
	Command             string `json:"command,omitempty"`
	NativeSessionID     string `json:"native_session_id,omitempty"`
	NativePolicyDigest  string `json:"native_policy_digest,omitempty"`
	NativeContextDigest string `json:"native_context_digest,omitempty"`
}

// BindRuntimeDirectTurn is idempotent for retries. /clear starts a new logical
// conversation but does not delete messages, memories or the audit trail.
func (tx *Tx) BindRuntimeDirectTurn(ctx context.Context, c RuntimeConfig, t RuntimeTask) (RuntimeDirectSession, error) {
	var out RuntimeDirectSession
	if _, err := RuntimeOwnerDirectProcessingRoute(ctx, tx.Conn, c, t.RouteID); err != nil {
		return out, err
	}
	if t.RuntimeID != c.ID {
		return out, Fail("denied", "direct turn belongs to another runtime")
	}
	err := tx.Conn.QueryRowContext(ctx, `SELECT dt.session_id,dt.command,s.native_session_id,s.native_policy_digest,s.native_context_digest
FROM runtime_direct_turns dt JOIN runtime_direct_sessions s ON s.id=dt.session_id WHERE dt.task_id=?`, t.ID).Scan(&out.ID, &out.Command, &out.NativeSessionID, &out.NativePolicyDigest, &out.NativeContextDigest)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if len(t.Messages) != 1 {
		return out, Fail("invalid_input", "direct turns require one original message")
	}
	m := t.Messages[0]
	valid, err := RuntimeOwnerDirectSenderCurrent(ctx, tx.Conn, c, m.ID)
	if err != nil {
		return out, err
	}
	if !valid || m.SelfAuthored || m.Sender != c.OwnerPrincipalID {
		return out, Fail("denied", "direct turn sender is not the owner")
	}
	out.Command = DirectCommand(m.Body)
	if out.Command == "clear" {
		_, err = tx.Conn.ExecContext(ctx, "UPDATE runtime_direct_sessions SET closed_at=? WHERE runtime_id=? AND route_id=? AND closed_at=''", Now(), c.ID, t.RouteID)
		if err != nil {
			return out, err
		}
	}
	err = tx.Conn.QueryRowContext(ctx, "SELECT id FROM runtime_direct_sessions WHERE runtime_id=? AND route_id=? AND closed_at=''", c.ID, t.RouteID).Scan(&out.ID)
	if errors.Is(err, sql.ErrNoRows) {
		out.ID = NewID()
		_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_direct_sessions(id,runtime_id,route_id,created_at) VALUES(?,?,?,?)", out.ID, c.ID, t.RouteID, Now())
	}
	if err != nil {
		return out, err
	}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_direct_turns(task_id,session_id,command) VALUES(?,?,?)", t.ID, out.ID, out.Command)
	return out, err
}

// PersistRuntimeDirectNativeSession records the native Claude session only after
// the corresponding reply has been accepted. The logical session remains the
// SQLite conversation identity; these fields are a resumable execution hint.
func (tx *Tx) PersistRuntimeDirectNativeSession(ctx context.Context, sessionID, nativeID, policyDigest, contextDigest string) error {
	if sessionID == "" || nativeID == "" || policyDigest == "" || contextDigest == "" {
		return Fail("invalid_input", "direct native session state is incomplete")
	}
	res, err := tx.Conn.ExecContext(ctx, `UPDATE runtime_direct_sessions
SET native_session_id=?,native_policy_digest=?,native_context_digest=?
WHERE id=? AND closed_at=''`, nativeID, policyDigest, contextDigest, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Fail("conflict", "direct session is no longer active")
	}
	return nil
}

func RuntimeDirectTurnCurrent(ctx context.Context, q Queryer, taskID string) (bool, error) {
	var closed int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_direct_turns dt
JOIN runtime_direct_sessions s ON s.id=dt.session_id WHERE dt.task_id=? AND s.closed_at<>''`, taskID).Scan(&closed)
	if err != nil || closed > 0 {
		return false, err
	}
	// A received /clear is a command barrier even when Claude is still answering.
	var resets int
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_tasks t
JOIN runtime_configs c ON c.id=t.runtime_id AND c.application_mode='direct'
JOIN channel_routes r ON r.id=t.route_id JOIN channels ch ON ch.id=c.channel_id
JOIN messages m ON m.channel_id=c.channel_id AND m.conversation_id=r.conversation_id
JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision
JOIN identity_aliases ia ON ia.tenant=ch.tenant AND ia.id_type=m.sender_id_type AND ia.id_value=m.sender_id_value AND ia.verified=1 AND ia.principal_id=c.owner_principal_id
WHERE t.id=? AND m.sender_principal=c.owner_principal_id AND m.self_authored=0 AND m.availability='available' AND m.context_only=0
AND trim(mr.body)='/clear' AND m.rowid>(SELECT max(mm.rowid) FROM runtime_task_messages tm JOIN messages mm ON mm.id=tm.message_id WHERE tm.task_id=t.id)`, taskID).Scan(&resets)
	return resets == 0, err
}

// Only successful, delivered turns from this logical conversation are used to
// reconstruct Agent context after a process restart. Commands are not turns.
func RuntimeDirectHistory(ctx context.Context, q Queryer, c RuntimeConfig, t RuntimeTask, sessionID string) ([]RuntimeMessage, error) {
	if t.RuntimeID != c.ID {
		return nil, Fail("denied", "direct history belongs to another runtime")
	}
	if _, err := RuntimeOwnerDirectProcessingRoute(ctx, q, c, t.RouteID); err != nil {
		return nil, err
	}
	var admitted int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_direct_turns dt
JOIN runtime_direct_sessions s ON s.id=dt.session_id
WHERE dt.task_id=? AND s.id=? AND s.runtime_id=? AND s.route_id=? AND s.closed_at=''`, t.ID, sessionID, c.ID, t.RouteID).Scan(&admitted); err != nil {
		return nil, err
	}
	if admitted != 1 {
		return nil, Fail("denied", "direct history requires the current task session")
	}
	// The application-bot conversation is the durable user-facing session.
	// The personal DWS retention epoch governs source evidence, not accepted
	// bot turns; applying it here would silently break private-chat resumption
	// when the seven-day source cleanup boundary advances.
	rows, err := q.QueryContext(ctx, `SELECT m.id,m.sent_at,m.current_revision,mr.body,rt.result FROM runtime_direct_turns dt
JOIN runtime_tasks rt ON rt.id=dt.task_id JOIN runtime_task_messages tm ON tm.task_id=rt.id
JOIN messages m ON m.id=tm.message_id JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision
JOIN channels ch ON ch.id=m.channel_id
JOIN identity_aliases ia ON ia.tenant=ch.tenant AND ia.id_type=m.sender_id_type AND ia.id_value=m.sender_id_value AND ia.verified=1 AND ia.principal_id=m.sender_principal
WHERE dt.session_id=? AND dt.command='' AND rt.runtime_id=? AND rt.route_id=? AND rt.kind<>'memory' AND rt.status IN ('completed','awaiting_confirmation','action_failed','action_unknown')
AND m.availability='available' AND m.current_revision=tm.revision AND m.sender_principal=? AND m.self_authored=0
AND dt.sequence<(SELECT sequence FROM runtime_direct_turns WHERE task_id=?)
AND EXISTS(SELECT 1 FROM outbox o WHERE o.job_id=rt.id AND o.state='accepted' AND o.reason NOT IN ('runtime_receipt','runtime_processing_receipt','runtime_completion_receipt','runtime_failure_receipt') AND o.route_id=?)
ORDER BY dt.sequence`, sessionID, c.ID, t.RouteID, c.OwnerPrincipalID, t.ID, c.DeliveryRouteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuntimeMessage{}
	for rows.Next() {
		var m RuntimeMessage
		var reply string
		if err = rows.Scan(&m.ID, &m.SentAt, &m.Revision, &m.Body, &reply); err != nil {
			return nil, err
		}
		m.Sender = c.OwnerPrincipalID
		out = append(out, m, RuntimeMessage{Sender: "bot", SelfAuthored: true, Body: reply, SentAt: m.SentAt})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].SelfAuthored {
			continue
		}
		out[i].Quote, err = runtimeMessageQuote(ctx, q, out[i].ID, out[i].Revision)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
