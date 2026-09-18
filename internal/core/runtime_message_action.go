package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// RuntimeMessageInput is an explicit Agent decision, never a runtime completion
// notification. Evidence lists every message disclosed in the outgoing content;
// an empty list is appropriate for generic, non-disclosing outreach.
type RuntimeMessageInput struct {
	AttemptID          string   `json:"attempt_id"`
	IdempotencyKey     string   `json:"idempotency_key"`
	TargetType         string   `json:"target_type"`
	TargetID           string   `json:"target_id"`
	Content            string   `json:"content"`
	Reason             string   `json:"reason"`
	EvidenceMessageIDs []string `json:"evidence_message_ids"`
}

// RuntimeMessageTargetVerification is supplied by the trusted CLI adapter after
// exact-ID resolution. It is deliberately not part of RuntimeMessageInput.
type RuntimeMessageTargetVerification struct {
	ChannelID      string
	ChannelVersion int
	OwnerProfile   string
	OwnerUserID    string
	TargetType     string
	TargetID       string
}

type RuntimeMessageAction struct {
	ID                       string         `json:"id"`
	TaskID                   string         `json:"task_id"`
	TaskVersion              int            `json:"task_version"`
	AttemptID                string         `json:"attempt_id"`
	ChannelID                string         `json:"channel_id"`
	ChannelVersion           int            `json:"channel_version"`
	OwnerProfile             string         `json:"owner_profile"`
	OwnerTenant              string         `json:"owner_tenant"`
	OwnerUserID              string         `json:"owner_user_id"`
	TargetType               string         `json:"target_type"`
	TargetID                 string         `json:"target_id"`
	Content                  string         `json:"content"`
	Reason                   string         `json:"reason"`
	EvidenceMessageIDs       []string       `json:"evidence_message_ids"`
	EvidenceMessageRevisions map[string]int `json:"evidence_message_revisions,omitempty"`
	InputDigest              string         `json:"input_digest"`
	IdempotencyKey           string         `json:"idempotency_key"`
	State                    string         `json:"state"`
	Receipt                  string         `json:"receipt,omitempty"`
	Detail                   string         `json:"detail,omitempty"`
	ProviderMessageIDs       []string       `json:"provider_message_ids,omitempty"`
	ProviderSendTaskID       string         `json:"provider_send_task_id,omitempty"`
	ProviderConversationID   string         `json:"provider_conversation_id,omitempty"`
	CreatedAt                string         `json:"created_at"`
	UpdatedAt                string         `json:"updated_at"`
}

const runtimeMessageActionColumns = "id,task_id,task_version,attempt_id,channel_id,channel_version,owner_profile,owner_tenant,owner_user_id,target_type,target_id,content,reason,evidence_message_ids,evidence_message_revisions,input_digest,idempotency_key,state,receipt,detail,provider_message_ids,provider_send_task_id,provider_conversation_id,created_at,updated_at"

func scanRuntimeMessageAction(row scanner) (RuntimeMessageAction, error) {
	var a RuntimeMessageAction
	var evidence, ids, revisions string
	err := row.Scan(&a.ID, &a.TaskID, &a.TaskVersion, &a.AttemptID, &a.ChannelID, &a.ChannelVersion, &a.OwnerProfile, &a.OwnerTenant, &a.OwnerUserID, &a.TargetType, &a.TargetID, &a.Content, &a.Reason, &evidence, &revisions, &a.InputDigest, &a.IdempotencyKey, &a.State, &a.Receipt, &a.Detail, &ids, &a.ProviderSendTaskID, &a.ProviderConversationID, &a.CreatedAt, &a.UpdatedAt)
	if err == nil {
		err = json.Unmarshal([]byte(evidence), &a.EvidenceMessageIDs)
	}
	if err == nil {
		err = json.Unmarshal([]byte(ids), &a.ProviderMessageIDs)
	}
	if err == nil {
		err = json.Unmarshal([]byte(revisions), &a.EvidenceMessageRevisions)
	}
	return a, err
}

func RuntimeMessageActions(ctx context.Context, q Queryer, taskID string) ([]RuntimeMessageAction, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+runtimeMessageActionColumns+" FROM runtime_message_actions WHERE task_id=? ORDER BY created_at,id", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuntimeMessageAction{}
	for rows.Next() {
		a, e := scanRuntimeMessageAction(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if err = redactRuntimeMessageAction(ctx, q, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func redactRuntimeMessageAction(ctx context.Context, q Queryer, a *RuntimeMessageAction) error {
	current, err := runtimeMessageActionSourcesCurrent(ctx, q, *a)
	if err != nil {
		return err
	}
	if !current {
		a.Content = ""
		a.Reason = ""
		a.Receipt = ""
		a.Detail = "source evidence is no longer available"
	}
	return nil
}

func runtimeMessageActionSourcesCurrent(ctx context.Context, q Queryer, a RuntimeMessageAction) (bool, error) {
	// A task may replace its source links when it advances. Current links do
	// not make an earlier attempt's communication safe to display again.
	var version int
	if err := q.QueryRowContext(ctx, "SELECT version FROM runtime_tasks WHERE id=?", a.TaskID).Scan(&version); err != nil {
		return false, err
	}
	if version != a.TaskVersion {
		return false, nil
	}
	current, err := RuntimeTaskOutputCurrent(ctx, q, a.TaskID)
	if err != nil {
		return false, err
	}
	for _, id := range a.EvidenceMessageIDs {
		var n int
		if err = q.QueryRowContext(ctx, "SELECT count(*) FROM messages m WHERE m.id=? AND m.current_revision=? AND m.availability='available' AND "+retainedMessagePredicate("m"), id, a.EvidenceMessageRevisions[id]).Scan(&n); err != nil {
			return false, err
		}
		current = current && n == 1
	}
	return current, nil
}

// UnresolvedRuntimeMessageActions supports read-only late receipt reconciliation
// before observation analysis. The internal view never includes message body,
// decision text or raw receipts. Native task IDs survive source retention, so
// a late receipt can identify an echo after its source text has been erased.
func UnresolvedRuntimeMessageActions(ctx context.Context, q Queryer, channelID string) ([]RuntimeMessageAction, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+runtimeMessageActionColumns+" FROM runtime_message_actions WHERE channel_id=? AND state IN ('accepted','unknown') AND provider_send_task_id<>'' AND json_array_length(provider_message_ids)=0 ORDER BY created_at LIMIT 100", channelID)
	if err != nil {
		return nil, err
	}
	out := []RuntimeMessageAction{}
	for rows.Next() {
		a, e := scanRuntimeMessageAction(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Content = ""
		out[i].Reason = ""
		out[i].EvidenceMessageIDs = nil
		out[i].EvidenceMessageRevisions = nil
		out[i].Detail = ""
		out[i].Receipt = JSON(map[string]string{"openTaskId": out[i].ProviderSendTaskID})
	}
	return out, nil
}

func validateRuntimeMessageInput(in RuntimeMessageInput) error {
	if !contains([]string{"group", "user"}, in.TargetType) || !identifier.MatchString(in.TargetID) || !identifier.MatchString(in.IdempotencyKey) || in.AttemptID == "" || strings.TrimSpace(in.Content) == "" || strings.TrimSpace(in.Reason) == "" || len([]rune(in.Content)) > 20000 || len([]rune(in.Reason)) > 2000 || len(in.EvidenceMessageIDs) > 100 {
		return Fail("invalid_input", "message requires an attempt, stable target, idempotency key, bounded content and decision reason")
	}
	return nil
}

func runtimeMessageDigest(taskID string, in RuntimeMessageInput) string {
	// Attempt changes do not permit a duplicate communication after a resume.
	in.AttemptID = ""
	return Digest(map[string]any{"task_id": taskID, "message": in})
}

// ExistingRuntimeMessageAction permits read-only recovery of an earlier result.
// A reused key with changed content, target or decision is always a conflict.
func ExistingRuntimeMessageAction(ctx context.Context, q Queryer, taskID string, in RuntimeMessageInput) (RuntimeMessageAction, bool, error) {
	if err := validateRuntimeMessageInput(in); err != nil {
		return RuntimeMessageAction{}, false, err
	}
	a, err := scanRuntimeMessageAction(q.QueryRowContext(ctx, "SELECT "+runtimeMessageActionColumns+" FROM runtime_message_actions WHERE channel_id=(SELECT c.channel_id FROM runtime_tasks t JOIN runtime_configs c ON c.id=t.runtime_id WHERE t.id=?) AND idempotency_key=?", taskID, in.IdempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	if a.InputDigest != runtimeMessageDigest(taskID, in) {
		return a, true, Fail("conflict", "idempotency key already belongs to a different communication")
	}
	err = redactRuntimeMessageAction(ctx, q, &a)
	return a, true, err
}

// AuthorizeRuntimeMessageAction records the send BEFORE calling DWS. A repeated
// key returns its existing result with created=false, including sending/unknown.
func (tx *Tx) AuthorizeRuntimeMessageAction(ctx context.Context, taskID string, in RuntimeMessageInput, verified RuntimeMessageTargetVerification) (RuntimeMessageAction, bool, error) {
	if a, found, err := ExistingRuntimeMessageAction(ctx, tx.Conn, taskID, in); err != nil || found {
		return a, false, err
	}
	t, err := ReadRuntimeTask(ctx, tx.Conn, taskID)
	if err != nil {
		return RuntimeMessageAction{}, false, err
	}
	c, err := ReadRuntime(ctx, tx.Conn, t.RuntimeID)
	if err != nil {
		return RuntimeMessageAction{}, false, err
	}
	if t.Status != "running" || c.Status != "running" || c.ApplicationMode != "proactive" {
		return RuntimeMessageAction{}, false, Fail("denied", "owner communication requires a running proactive task")
	}
	if err = tx.CheckRuntimeAttemptPolicy(ctx, in.AttemptID, t.ID, t.Version); err != nil {
		return RuntimeMessageAction{}, false, err
	}
	policy, err := ResolveRuntimeTaskAgent(ctx, tx.Conn, c, t)
	if err != nil {
		return RuntimeMessageAction{}, false, err
	}
	if policy.ExternalActions != "owner_delegated" {
		return RuntimeMessageAction{}, false, Fail("denied", "owner has not delegated autonomous communication to this Agent")
	}
	if current, e := runtimeTaskMessagesCurrent(ctx, tx.Conn, t.ID); e != nil || !current {
		if e != nil {
			return RuntimeMessageAction{}, false, e
		}
		return RuntimeMessageAction{}, false, Fail("conflict", "task sources changed before communication")
	}
	ch, err := ReadChannel(ctx, tx.Conn, c.ChannelID)
	if err != nil {
		return RuntimeMessageAction{}, false, err
	}
	if ch.Kind != ChannelDwsPersonal || !contains([]string{"configured", "active"}, ch.Status) || ch.Identity.Profile == "" || ch.Identity.ExpectedUserID == "" || verified.ChannelID != ch.ID || verified.ChannelVersion != ch.ConfigVersion || verified.OwnerProfile != ch.Identity.Profile || verified.OwnerUserID != ch.Identity.ExpectedUserID || verified.TargetID != in.TargetID || verified.TargetType != in.TargetType {
		return RuntimeMessageAction{}, false, Fail("denied", "authenticated DWS owner or verified target changed")
	}
	var owner int
	if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM identity_aliases WHERE tenant=? AND id_type='user_id' AND id_value=? AND principal_id=? AND verified=1 AND basis='authenticated_dws_profile'", ch.Tenant, ch.Identity.ExpectedUserID, c.OwnerPrincipalID).Scan(&owner); err != nil {
		return RuntimeMessageAction{}, false, err
	}
	if owner != 1 {
		return RuntimeMessageAction{}, false, Fail("denied", "runtime owner is not the verified DWS owner")
	}
	if in.TargetType == "group" {
		var blocked int
		if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM channel_routes WHERE channel_id=? AND conversation_id=? AND (status<>'active' OR mode='ignore' OR conversation_type<>'group')", ch.ID, in.TargetID).Scan(&blocked); err != nil {
			return RuntimeMessageAction{}, false, err
		}
		if blocked != 0 {
			return RuntimeMessageAction{}, false, Fail("denied", "target group is explicitly excluded")
		}
	}
	evidenceRevisions := map[string]int{}
	for _, id := range in.EvidenceMessageIDs {
		m, e := ReadMessage(ctx, tx.Conn, id)
		if e != nil {
			return RuntimeMessageAction{}, false, e
		}
		if m.ChannelID != ch.ID || m.Availability != "available" || m.Body == "" {
			return RuntimeMessageAction{}, false, Fail("denied", "communication evidence is unavailable or belongs to another account")
		}
		evidenceRevisions[id] = m.Revision
		var retained int
		if e = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM messages m WHERE m.id=? AND "+retainedMessagePredicate("m"), id).Scan(&retained); e != nil {
			return RuntimeMessageAction{}, false, e
		}
		if retained != 1 {
			return RuntimeMessageAction{}, false, Fail("denied", "communication evidence has expired")
		}
		allowed := in.TargetType == "group" && m.Conversation == in.TargetID
		if in.TargetType == "user" {
			allowed = in.TargetID == ch.Identity.ExpectedUserID
			if !allowed {
				var n int
				e = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM identity_aliases WHERE tenant=? AND id_type='user_id' AND id_value=? AND principal_id=? AND verified=1", ch.Tenant, in.TargetID, m.Sender).Scan(&n)
				if e != nil {
					return RuntimeMessageAction{}, false, e
				}
				allowed = n == 1
			}
		}
		if !allowed {
			return RuntimeMessageAction{}, false, Fail("denied", "reading evidence does not authorize disclosing it to this audience")
		}
	}
	a := RuntimeMessageAction{ID: NewID(), TaskID: t.ID, TaskVersion: t.Version, AttemptID: in.AttemptID, ChannelID: ch.ID, ChannelVersion: ch.ConfigVersion, OwnerProfile: ch.Identity.Profile, OwnerTenant: ch.Tenant, OwnerUserID: ch.Identity.ExpectedUserID, TargetType: in.TargetType, TargetID: in.TargetID, Content: in.Content, Reason: in.Reason, EvidenceMessageIDs: in.EvidenceMessageIDs, EvidenceMessageRevisions: evidenceRevisions, InputDigest: runtimeMessageDigest(taskID, in), IdempotencyKey: in.IdempotencyKey, State: "sending", ProviderMessageIDs: []string{}, CreatedAt: Now(), UpdatedAt: Now()}
	if a.EvidenceMessageIDs == nil {
		a.EvidenceMessageIDs = []string{}
	}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_message_actions("+runtimeMessageActionColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", a.ID, a.TaskID, a.TaskVersion, a.AttemptID, a.ChannelID, a.ChannelVersion, a.OwnerProfile, a.OwnerTenant, a.OwnerUserID, a.TargetType, a.TargetID, a.Content, a.Reason, JSON(a.EvidenceMessageIDs), JSON(a.EvidenceMessageRevisions), a.InputDigest, a.IdempotencyKey, a.State, a.Receipt, a.Detail, JSON(a.ProviderMessageIDs), a.ProviderSendTaskID, a.ProviderConversationID, a.CreatedAt, a.UpdatedAt)
	return a, err == nil, err
}

func (tx *Tx) RecordRuntimeMessageResult(ctx context.Context, id, state, receipt, detail string) (RuntimeMessageAction, error) {
	if !contains([]string{"accepted", "failed", "unknown"}, state) {
		return RuntimeMessageAction{}, Fail("invalid_input", "message result must be accepted, failed or unknown")
	}
	if state == "accepted" && strings.TrimSpace(receipt) == "" {
		return RuntimeMessageAction{}, Fail("invalid_input", "accepted message requires platform evidence")
	}
	ids := runtimeReceiptMessageIDs(receipt)
	sendTaskID, conversationID := runtimeReceiptSendTaskID(receipt), runtimeReceiptConversationID(receipt)
	storedReceipt, storedDetail, err := runtimeMessageReceiptStorage(ctx, tx.Conn, id, receipt, detail)
	if err != nil {
		return RuntimeMessageAction{}, err
	}
	// Configuration changes may mark an in-flight send unknown before its
	// actual transport result returns. Recording that fact does not resend.
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_message_actions SET state=?,receipt=?,detail=?,provider_message_ids=?,provider_send_task_id=?,provider_conversation_id=?,updated_at=? WHERE id=? AND (state='sending' OR (state='unknown' AND receipt='' AND provider_send_task_id='' AND json_array_length(provider_message_ids)=0))", state, storedReceipt, storedDetail, JSON(ids), sendTaskID, conversationID, Now(), id)
	if err != nil {
		return RuntimeMessageAction{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return RuntimeMessageAction{}, Fail("conflict", "message result has already been recorded")
	}
	a, err := scanRuntimeMessageAction(tx.Conn.QueryRowContext(ctx, "SELECT "+runtimeMessageActionColumns+" FROM runtime_message_actions WHERE id=?", id))
	if err != nil {
		return a, err
	}
	err = redactRuntimeMessageAction(ctx, tx.Conn, &a)
	return a, err
}

// Receipt reconciliation only adds platform facts. It cannot return an action
// to sending, spend a new permission or cause a transport call.
func (tx *Tx) ReconcileRuntimeMessageResult(ctx context.Context, id, state, receipt, detail string) (RuntimeMessageAction, error) {
	if !contains([]string{"accepted", "failed", "unknown"}, state) || receipt == "" {
		return RuntimeMessageAction{}, Fail("invalid_input", "reconciliation requires a platform receipt and terminal state")
	}
	sendTaskID, conversationID := runtimeReceiptSendTaskID(receipt), runtimeReceiptConversationID(receipt)
	storedReceipt, storedDetail, err := runtimeMessageReceiptStorage(ctx, tx.Conn, id, receipt, detail)
	if err != nil {
		return RuntimeMessageAction{}, err
	}
	res, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_message_actions SET state=?,receipt=?,detail=?,provider_message_ids=?,provider_send_task_id=CASE WHEN ?<>'' THEN ? ELSE provider_send_task_id END,provider_conversation_id=CASE WHEN ?<>'' THEN ? ELSE provider_conversation_id END,updated_at=? WHERE id=? AND state IN ('accepted','unknown') AND json_array_length(provider_message_ids)=0", state, storedReceipt, storedDetail, JSON(runtimeReceiptMessageIDs(receipt)), sendTaskID, sendTaskID, conversationID, conversationID, Now(), id)
	if err != nil {
		return RuntimeMessageAction{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return RuntimeMessageAction{}, Fail("conflict", "communication has already been reconciled")
	}
	a, err := scanRuntimeMessageAction(tx.Conn.QueryRowContext(ctx, "SELECT "+runtimeMessageActionColumns+" FROM runtime_message_actions WHERE id=?", id))
	if err != nil {
		return a, err
	}
	err = redactRuntimeMessageAction(ctx, tx.Conn, &a)
	return a, err
}

func runtimeMessageReceiptStorage(ctx context.Context, q Queryer, id, receipt, detail string) (string, string, error) {
	a, err := scanRuntimeMessageAction(q.QueryRowContext(ctx, "SELECT "+runtimeMessageActionColumns+" FROM runtime_message_actions WHERE id=?", id))
	if err != nil {
		return "", "", err
	}
	current, err := runtimeMessageActionSourcesCurrent(ctx, q, a)
	if err != nil {
		return "", "", err
	}
	if !current {
		return "", "source evidence is no longer available", nil
	}
	return receipt, detail, nil
}

func runtimeReceiptSendTaskID(receipt string) string {
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(receipt), &raw) != nil {
		return ""
	}
	if send, ok := raw["send"]; ok {
		if json.Unmarshal(send, &raw) != nil {
			return ""
		}
	}
	var id string
	if json.Unmarshal(raw["openTaskId"], &id) == nil && id != "" {
		return id
	}
	var nested struct {
		OpenTaskID string `json:"openTaskId"`
	}
	if json.Unmarshal(raw["result"], &nested) == nil {
		return nested.OpenTaskID
	}
	return ""
}

func runtimeReceiptConversationID(receipt string) string {
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(receipt), &raw) != nil {
		return ""
	}
	if delivery, ok := raw["delivery"]; ok {
		if json.Unmarshal(delivery, &raw) != nil {
			return ""
		}
	}
	var ids = []string{}
	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if k == "openConversationId" || k == "conversationId" {
					if id, ok := v.(string); ok && id != "" {
						ids = append(ids, id)
					}
				} else {
					collect(v)
				}
			}
		case []any:
			for _, v := range x {
				collect(v)
			}
		}
	}
	var value any
	b, _ := json.Marshal(raw)
	if json.Unmarshal(b, &value) != nil {
		return ""
	}
	collect(value)
	if len(ids) == 0 {
		return ""
	}
	for _, id := range ids {
		if id != ids[0] {
			return ""
		}
	}
	return ids[0]
}

// Only explicit provider MESSAGE IDs suppress echoes. Task IDs, receipt IDs,
// content equality and self_authored alone cannot distinguish a human owner.
func runtimeReceiptMessageIDs(receipt string) []string {
	out := []string{}
	seen := map[string]bool{}
	var raw any
	if json.Unmarshal([]byte(receipt), &raw) != nil {
		return out
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if k == "messageId" || k == "openMessageId" || k == "msgId" {
					if id, ok := v.(string); ok && id != "" && !seen[id] {
						seen[id] = true
						out = append(out, id)
					}
				} else {
					walk(v)
				}
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(raw)
	return out
}

func RuntimeAgentMessageEcho(ctx context.Context, q Queryer, channelID, conversationID, providerMessageID string) (bool, error) {
	if providerMessageID == "" {
		return false, nil
	}
	var n int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM runtime_message_actions a,json_each(a.provider_message_ids) p WHERE a.channel_id=? AND a.state IN ('accepted','failed','unknown') AND p.value=? AND "+runtimeMessageTargetConversationPredicate, channelID, providerMessageID, conversationID, conversationID, conversationID).Scan(&n)
	return n > 0, err
}

// AwaitingReceipt defers only owner-authored observations in an exactly known
// target conversation until native message IDs can separate Agent and human.
// A contact name or equal-looking identifier in another namespace is no proof.
func RuntimeAgentMessageAwaitingReceipt(ctx context.Context, q Queryer, channelID, conversationID, sentAt string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_message_actions a
WHERE a.channel_id=? AND a.state IN ('sending','unknown','accepted')
AND json_array_length(a.provider_message_ids)=0 AND strftime('%s',a.created_at)<=strftime('%s',?)
AND `+runtimeMessageTargetConversationPredicate, channelID, sentAt, conversationID, conversationID, conversationID).Scan(&n)
	return n > 0, err
}

const runtimeMessageTargetConversationPredicate = `((a.target_type='group' AND a.target_id=?) OR (a.target_type='user' AND (a.provider_conversation_id=? OR EXISTS(
SELECT 1 FROM direct_conversation_contacts dc JOIN channels c ON c.id=dc.channel_id
WHERE dc.channel_id=a.channel_id AND dc.conversation_id=? AND (
(dc.peer_id_type='user_id' AND dc.peer_id_value=a.target_id) OR EXISTS(
SELECT 1 FROM identity_aliases peer JOIN identity_aliases target ON target.tenant=peer.tenant AND target.principal_id=peer.principal_id
WHERE peer.tenant=c.tenant AND peer.id_type=dc.peer_id_type AND peer.id_value=dc.peer_id_value AND peer.verified=1
AND target.id_type='user_id' AND target.id_value=a.target_id AND target.verified=1))))))`
