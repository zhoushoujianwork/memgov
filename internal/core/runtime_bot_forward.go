package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

const RuntimeBotForwardAction = "bot_forward_message"

type RuntimeBotForwardPayload struct {
	Target        string `json:"target"`
	RecipientName string `json:"recipient_name"`
	Content       string `json:"content"`
}

func ParseRuntimeBotForward(action RuntimePendingAction) (RuntimeBotForwardPayload, error) {
	var payload RuntimeBotForwardPayload
	if action.Kind != RuntimeBotForwardAction || json.Unmarshal([]byte(action.Payload), &payload) != nil || payload.Target != action.Target || strings.TrimSpace(payload.Content) == "" || len([]rune(payload.Content)) > 20000 || Hash([]byte(action.Payload)) != action.PayloadDigest {
		return payload, Fail("denied", "bot forwarding target or content differs from the approved payload")
	}
	return payload, nil
}

func runtimeActionDisplayPayload(action RuntimePendingAction) string {
	if action.Kind == RuntimeBotForwardAction {
		if payload, err := ParseRuntimeBotForward(action); err == nil {
			return payload.Content
		}
	}
	return action.Payload
}

func runtimeActionDisplayTarget(action RuntimePendingAction) string {
	if action.Kind == RuntimeBotForwardAction {
		if payload, err := ParseRuntimeBotForward(action); err == nil && payload.RecipientName != "" {
			return payload.RecipientName + " (" + action.Target + ")"
		}
	}
	return action.Target
}

type RuntimeBotTargetVerification struct {
	ChannelID      string
	ChannelVersion int
	Tenant         string
	TargetType     string
	TargetID       string
}

// AuthorizeRuntimeBotMessage records a bot MCP send before the transport call.
// A repeated call with the same target and content returns the prior outcome
// and never starts a second send.
func (tx *Tx) AuthorizeRuntimeBotMessage(ctx context.Context, taskID string, in RuntimeMessageInput, verified RuntimeBotTargetVerification) (RuntimeMessageAction, bool, error) {
	if err := validateRuntimeMessageInput(in); err != nil {
		return RuntimeMessageAction{}, false, err
	}
	if in.Reason != "group_bot_forward" || len(in.EvidenceMessageIDs) != 0 {
		return RuntimeMessageAction{}, false, Fail("invalid_input", "bot MCP send has an invalid decision record")
	}
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
	app, err := ReadChannel(ctx, tx.Conn, c.ChannelID)
	if err != nil {
		return RuntimeMessageAction{}, false, err
	}
	if t.Status != "running" || c.Status != "running" || c.ApplicationMode != "group_mention" || app.Kind != ChannelDingTalkApp || !app.Capabilities.Verified["send"] || !contains([]string{"configured", "active"}, app.Status) {
		return RuntimeMessageAction{}, false, Fail("denied", "bot forwarding requires an active group bot")
	}
	if verified.ChannelID != app.ID || verified.ChannelVersion != app.ConfigVersion || verified.Tenant != app.Tenant || verified.TargetType != in.TargetType || verified.TargetID != in.TargetID {
		return RuntimeMessageAction{}, false, Fail("denied", "bot recipient verification changed")
	}
	if err = tx.CheckRuntimeAttemptPolicy(ctx, in.AttemptID, t.ID, t.Version); err != nil {
		return RuntimeMessageAction{}, false, err
	}
	if current, e := runtimeTaskMessagesCurrent(ctx, tx.Conn, t.ID); e != nil || !current {
		if e != nil {
			return RuntimeMessageAction{}, false, e
		}
		return RuntimeMessageAction{}, false, Fail("conflict", "bot forwarding source changed")
	}
	if admitted, e := runtimeTaskTriggerAdmitted(ctx, tx.Conn, c, t); e != nil || !admitted {
		if e != nil {
			return RuntimeMessageAction{}, false, e
		}
		return RuntimeMessageAction{}, false, Fail("denied", "bot forwarding trigger is no longer admitted")
	}
	if in.TargetType == "group" {
		route, e := RouteFor(ctx, tx.Conn, app.ID, in.TargetID)
		if e != nil || route.Status != "active" || route.ConversationType != "group" || route.Mode != "assistant" || !contains(c.RouteIDs, route.ID) {
			return RuntimeMessageAction{}, false, Fail("denied", "target group is not mounted on this bot")
		}
	}
	now := Now()
	a := RuntimeMessageAction{ID: NewID(), TaskID: t.ID, TaskVersion: t.Version, AttemptID: in.AttemptID, ChannelID: app.ID, ChannelVersion: app.ConfigVersion, OwnerTenant: app.Tenant, TargetType: in.TargetType, TargetID: in.TargetID, Content: in.Content, Reason: in.Reason, EvidenceMessageIDs: []string{}, EvidenceMessageRevisions: map[string]int{}, InputDigest: runtimeMessageDigest(taskID, in), IdempotencyKey: in.IdempotencyKey, State: "sending", ProviderMessageIDs: []string{}, CreatedAt: now, UpdatedAt: now}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_message_actions("+runtimeMessageActionColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", a.ID, a.TaskID, a.TaskVersion, a.AttemptID, a.ChannelID, a.ChannelVersion, a.OwnerProfile, a.OwnerTenant, a.OwnerUserID, a.TargetType, a.TargetID, a.Content, a.Reason, JSON(a.EvidenceMessageIDs), JSON(a.EvidenceMessageRevisions), a.InputDigest, a.IdempotencyKey, a.State, a.Receipt, a.Detail, JSON(a.ProviderMessageIDs), a.ProviderSendTaskID, a.ProviderConversationID, a.CreatedAt, a.UpdatedAt)
	return a, err == nil, err
}

// Recheck text confirmation immediately before a bot send. Card confirmations
// use the existing immutable card snapshot check in the policy gate.
func runtimeBotForwardApprovalCurrent(ctx context.Context, q Queryer, actionID string) error {
	var action RuntimePendingAction
	var task RuntimeTask
	var cfg RuntimeConfig
	var err error
	if err = q.QueryRowContext(ctx, "SELECT id,task_id,task_version,kind,target,payload,payload_digest,status,confirmed_by,confirmation_origin,created_at FROM runtime_pending_actions WHERE id=?", actionID).Scan(&action.ID, &action.TaskID, &action.TaskVersion, &action.Kind, &action.Target, &action.Payload, &action.PayloadDigest, &action.Status, &action.ConfirmedBy, &action.ConfirmationOrigin, &action.CreatedAt); err != nil {
		return err
	}
	task, err = ReadRuntimeTask(ctx, q, action.TaskID)
	if err != nil {
		return err
	}
	cfg, err = ReadRuntime(ctx, q, task.RuntimeID)
	if err != nil {
		return err
	}
	if action.Kind != RuntimeBotForwardAction || action.Status != "executing" || task.Status != "awaiting_confirmation" || action.TaskVersion != task.Version || action.ConfirmedBy != cfg.OwnerPrincipalID || cfg.ApplicationMode != "group_mention" {
		return Fail("denied", "bot forwarding approval changed")
	}
	if _, err = ParseRuntimeBotForward(action); err != nil {
		return err
	}
	if !strings.HasPrefix(action.ConfirmationOrigin, "dingtalk_message:") {
		return nil // Card approval is checked by runtimeCardActionCurrent.
	}
	messageID := strings.TrimPrefix(action.ConfirmationOrigin, "dingtalk_message:")
	var channelID, conversationID, body, sentAt string
	var addressed int
	err = q.QueryRowContext(ctx, `SELECT m.channel_id,m.conversation_id,mr.body,m.sent_at,m.addressed FROM messages m JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision WHERE m.id=? AND m.availability='available' AND m.context_only=0`, messageID).Scan(&channelID, &conversationID, &body, &sentAt, &addressed)
	if err != nil {
		if err == sql.ErrNoRows {
			return Fail("denied", "bot forwarding approval is unavailable")
		}
		return err
	}
	if channelID != cfg.ChannelID || strings.TrimSpace(runtimeGroupConfirmationBody(body, addressed != 0)) != ConfirmationToken(action) {
		return Fail("denied", "bot forwarding approval content or channel changed")
	}
	route, err := ReadRoute(ctx, q, task.RouteID)
	if err != nil {
		return err
	}
	if route.ConversationID != conversationID || route.Status != "active" {
		return Fail("denied", "bot forwarding approval left its origin group")
	}
	verified, err := runtimeConfirmationSenderCurrent(ctx, q, channelID, messageID, cfg.OwnerPrincipalID)
	if err != nil || !verified {
		return Fail("denied", "bot forwarding Owner identity is no longer verified")
	}
	sent, sentErr := time.Parse(time.RFC3339Nano, sentAt)
	created, createdErr := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if sentErr != nil || createdErr != nil || sent.Before(created) {
		return Fail("denied", "bot forwarding approval predates the action")
	}
	return nil
}

// ProposeRuntimeBotForward freezes a bot message for the existing group Owner
// confirmation. The caller must first resolve a user through the bound tenant
// directory, or a group through this bot's active mounted routes.
func (tx *Tx) ProposeRuntimeBotForward(ctx context.Context, taskID, attemptID, targetType, targetID, recipientName, content string, verified RuntimeBotTargetVerification) (map[string]any, error) {
	t, err := ReadRuntimeTask(ctx, tx.Conn, taskID)
	if err != nil {
		return nil, err
	}
	c, err := ReadRuntime(ctx, tx.Conn, t.RuntimeID)
	if err != nil {
		return nil, err
	}
	app, err := ReadChannel(ctx, tx.Conn, c.ChannelID)
	if err != nil {
		return nil, err
	}
	if t.Status != "running" || c.Status != "running" || c.ApplicationMode != "group_mention" || c.ExternalActions != "owner_confirmation" || app.Kind != ChannelDingTalkApp || !app.Capabilities.Verified["send"] || !contains([]string{"configured", "active"}, app.Status) {
		return nil, Fail("denied", "bot forwarding requires an active group bot with Owner confirmation")
	}
	if verified.ChannelID != app.ID || verified.ChannelVersion != app.ConfigVersion || verified.Tenant != app.Tenant || verified.TargetType != targetType || verified.TargetID != targetID {
		return nil, Fail("denied", "bot recipient verification does not match the current application")
	}
	if !contains([]string{"user", "group"}, targetType) || !identifier.MatchString(targetID) || strings.TrimSpace(recipientName) == "" || len([]rune(recipientName)) > 100 || strings.TrimSpace(content) == "" || len([]rune(content)) > 20000 {
		return nil, Fail("invalid_input", "bot forwarding requires a stable recipient and bounded message content")
	}
	if err = tx.CheckRuntimeAttemptPolicy(ctx, attemptID, t.ID, t.Version); err != nil {
		return nil, err
	}
	if current, e := runtimeTaskMessagesCurrent(ctx, tx.Conn, t.ID); e != nil || !current {
		if e != nil {
			return nil, e
		}
		return nil, Fail("conflict", "bot forwarding source changed")
	}
	if admitted, e := runtimeTaskTriggerAdmitted(ctx, tx.Conn, c, t); e != nil || !admitted {
		if e != nil {
			return nil, e
		}
		return nil, Fail("denied", "bot forwarding trigger is no longer admitted")
	}
	if targetType == "group" {
		route, e := RouteFor(ctx, tx.Conn, app.ID, targetID)
		if e != nil || route.Status != "active" || route.ConversationType != "group" || route.Mode != "assistant" || !contains(c.RouteIDs, route.ID) {
			return nil, Fail("denied", "target group is not mounted on this bot")
		}
	}
	target := targetType + ":" + targetID
	payload := JSON(RuntimeBotForwardPayload{Target: target, RecipientName: recipientName, Content: content})
	var existing string
	err = tx.Conn.QueryRowContext(ctx, "SELECT coalesce((SELECT id FROM runtime_pending_actions WHERE task_id=? AND task_version=? AND kind=? AND target=? AND payload_digest=? AND status='pending' LIMIT 1),'')", t.ID, t.Version, RuntimeBotForwardAction, target, Hash([]byte(payload))).Scan(&existing)
	if err != nil {
		return nil, err
	}
	if existing != "" {
		return map[string]any{"action_id": existing, "status": "pending_confirmation", "target_type": targetType, "target_id": targetID}, nil
	}
	var count int
	if err = tx.Conn.QueryRowContext(ctx, "SELECT count(*) FROM runtime_pending_actions WHERE task_id=? AND task_version=? AND status='pending'", t.ID, t.Version).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 20 {
		return nil, Fail("invalid_input", "too many pending operations")
	}
	id, now := NewID(), Now()
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO runtime_pending_actions(id,task_id,task_version,kind,target,payload,payload_digest,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?)", id, t.ID, t.Version, RuntimeBotForwardAction, target, payload, Hash([]byte(payload)), now, now)
	return map[string]any{"action_id": id, "status": "pending_confirmation", "target_type": targetType, "target_id": targetID}, err
}
