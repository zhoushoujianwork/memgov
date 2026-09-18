package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RuntimeCard is a frozen delivery, never model-selected recipients or actions.
// Card callbacks identify this snapshot by its outbox ID, not by button params.
type RuntimeCard struct {
	Text        string               `json:"text"`
	Title       string               `json:"title,omitempty"`
	Summary     string               `json:"summary,omitempty"`
	Details     string               `json:"details,omitempty"`
	TemplateID  string               `json:"template_id,omitempty"`
	TaskVersion int                  `json:"task_version"`
	Mentions    []RuntimeCardMention `json:"mentions,omitempty"`
	Actions     []RuntimeCardAction  `json:"actions,omitempty"`
}
type RuntimeCardMention struct {
	IDType  string `json:"id_type"`
	IDValue string `json:"id_value"`
	Name    string `json:"name"`
}
type RuntimeCardAction struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
}
type RuntimeCardCallback struct {
	EventID    string
	CorpID     string
	CardID     string
	SpaceID    string
	SpaceType  string
	UserID     string
	UserIDType int
	Action     string
}

func runtimeConfirmationCardPresentation(task RuntimeTask) (title, summary, details string) {
	title = strings.Join(strings.Fields(task.Title), " ")
	pending := make([]RuntimePendingAction, 0, len(task.Actions))
	for _, action := range task.Actions {
		if action.Status == "pending" {
			pending = append(pending, action)
		}
	}
	summary = fmt.Sprintf("待确认后执行 · %d 项操作", len(pending))
	var lines []string
	for i, action := range pending {
		if len(pending) > 1 {
			lines = append(lines, fmt.Sprintf("操作 %d · %s", i+1, strings.Join(strings.Fields(action.Kind), " ")))
		}
		if target := strings.Join(strings.Fields(action.Target), " "); target != "" {
			lines = append(lines, "目标："+target)
		}
		if payload := strings.Join(strings.Fields(action.Payload), " "); payload != "" {
			lines = append(lines, "内容："+payload)
		}
		if i+1 < len(pending) {
			lines = append(lines, "")
		}
	}
	details = strings.Join(lines, "\n")
	return title, summary, details
}

func runtimeCardMentionsEqual(left, right []RuntimeCardMention) bool {
	if len(left) != len(right) {
		return false
	}
	for i, m := range left {
		if m != right[i] {
			return false
		}
	}
	return true
}

// Recheck the approved snapshot immediately before every external execution,
// including after a restart. A later edit cannot inherit the owner's approval.
func runtimeCardActionCurrent(ctx context.Context, q Queryer, c RuntimeConfig, a RuntimePendingAction) error {
	if _, err := runtimeCardOwner(ctx, q, c); err != nil {
		return err
	}
	cardID, _, ok := strings.Cut(strings.TrimPrefix(a.ConfirmationOrigin, "dingtalk_card:"), ":")
	if !ok || a.ConfirmedBy != c.OwnerPrincipalID || c.Status != "running" || c.ApplicationMode != "group_mention" {
		return Fail("denied", "card approval owner or runtime changed")
	}
	t, err := ReadRuntimeTask(ctx, q, a.TaskID)
	if err != nil {
		return err
	}
	current, err := runtimeTaskMessagesCurrent(ctx, q, t.ID)
	if err != nil {
		return err
	}
	admitted, err := runtimeTaskTriggerAdmitted(ctx, q, c, t)
	if err != nil {
		return err
	}
	r, err := ReadRoute(ctx, q, t.RouteID)
	if err != nil {
		return err
	}
	var content string
	err = q.QueryRowContext(ctx, `SELECT content FROM outbox WHERE id=? AND job_id=? AND channel_id=? AND route_id=? AND conversation_id=? AND format='confirmation_card' AND state='accepted'`, cardID, t.ID, c.ChannelID, r.ID, r.ConversationID).Scan(&content)
	if err != nil {
		return Fail("conflict", "approved card delivery is no longer current")
	}
	var card RuntimeCard
	app, err := ReadChannel(ctx, q, c.ChannelID)
	if err != nil {
		return err
	}
	if !current || !admitted || t.Status != "awaiting_confirmation" || a.TaskVersion != t.Version || r.ConversationType != "group" || r.Mode != "assistant" || r.SendPolicy != "reply_to_trigger" || !contains(r.Triggers, "mention") || json.Unmarshal([]byte(content), &card) != nil || card.TaskVersion != t.Version || card.TemplateID != app.Identity.ConfirmationCardTemplate {
		return Fail("conflict", "approved card, task, source or route changed before execution")
	}
	for _, displayed := range card.Actions {
		if displayed.ID == a.ID && displayed.Digest == a.PayloadDigest && Hash([]byte(a.Payload)) == displayed.Digest && displayed.Kind == a.Kind && displayed.Target == a.Target {
			return nil
		}
	}
	return Fail("conflict", "approved action differs from the displayed card")
}

func runtimeCardOwner(ctx context.Context, q Queryer, c RuntimeConfig) (RuntimeCardMention, error) {
	history, err := ReadChannel(ctx, q, c.ContextChannelID)
	if err != nil {
		return RuntimeCardMention{}, err
	}
	app, err := ReadChannel(ctx, q, c.ChannelID)
	if err != nil {
		return RuntimeCardMention{}, err
	}
	bound, err := ReadChannel(ctx, q, app.Identity.HistoryChannel)
	if err != nil || bound.ID != history.ID || app.Kind != ChannelDingTalkApp || !contains([]string{"configured", "active"}, app.Status) {
		return RuntimeCardMention{}, Fail("denied", "card owner requires the application's explicit DWS history binding")
	}
	if history.Kind != ChannelDwsPersonal || history.Tenant != app.Tenant || !contains([]string{"configured", "active"}, history.Status) || history.Identity.ExpectedUserID == "" {
		return RuntimeCardMention{}, Fail("denied", "card approval requires the authenticated same-enterprise DWS owner")
	}
	var n int
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM identity_aliases WHERE tenant=? AND id_type='user_id' AND id_value=? AND verified=1 AND basis='authenticated_dws_profile' AND principal_id=?`, app.Tenant, history.Identity.ExpectedUserID, c.OwnerPrincipalID).Scan(&n)
	if err != nil {
		return RuntimeCardMention{}, err
	}
	if n != 1 {
		return RuntimeCardMention{}, Fail("denied", "DWS owner identity is no longer verified")
	}
	return RuntimeCardMention{IDType: "user_id", IDValue: history.Identity.ExpectedUserID, Name: "DWS 所有者"}, nil
}

func runtimeCardMentions(ctx context.Context, q Queryer, c RuntimeConfig, t RuntimeTask, requireOwner bool) ([]RuntimeCardMention, error) {
	mentions := []RuntimeCardMention{}
	for i := len(t.Messages) - 1; i >= 0; i-- {
		m := t.Messages[i]
		if m.SelfAuthored {
			continue
		}
		var typ, value, snapshot string
		err := q.QueryRowContext(ctx, `SELECT m.sender_id_type,m.sender_id_value,coalesce(s.content,'') FROM messages m LEFT JOIN source_origins so ON so.message_id=m.id AND so.revision=m.current_revision LEFT JOIN sources s ON s.id=so.source_id WHERE m.id=? AND m.channel_id=?`, m.ID, c.ChannelID).Scan(&typ, &value, &snapshot)
		if err != nil {
			return nil, err
		}
		// Only the provider's typed address; never infer an ID from a nickname.
		if contains([]string{"user_id", "union_id"}, typ) && value != "" {
			if requireOwner && typ == "union_id" {
				var mapped string
				var count int
				err = q.QueryRowContext(ctx, `SELECT count(*),coalesce(min(u.id_value),'') FROM identity_aliases a JOIN identity_aliases u ON u.tenant=a.tenant AND u.principal_id=a.principal_id AND u.verified=1 AND u.id_type='user_id' WHERE a.tenant=(SELECT tenant FROM channels WHERE id=?) AND a.id_type='union_id' AND a.id_value=? AND a.verified=1`, c.ChannelID, value).Scan(&count, &mapped)
				if err != nil {
					return nil, err
				}
				if count != 1 {
					return nil, Fail("denied", "confirmation card requester has no unique verified user ID")
				}
				typ, value = "user_id", mapped
			}
			name := strings.TrimSpace(messageSnapshotField(snapshot, "sender_display_name"))
			if name == "" || len([]rune(name)) > 100 {
				name = "需求发起人"
			}
			mentions = append(mentions, RuntimeCardMention{IDType: typ, IDValue: value, Name: name})
		}
		if requireOwner && len(mentions) == 0 {
			return nil, Fail("denied", "confirmation card requester has no provider user address")
		}
		break
	}
	if t.Status == "awaiting_confirmation" {
		owner, err := runtimeCardOwner(ctx, q, c)
		if err != nil && requireOwner {
			return nil, err
		}
		if err == nil {
			found := false
			for _, m := range mentions {
				if m.IDType == owner.IDType && m.IDValue == owner.IDValue {
					found = true
				}
			}
			if !found {
				mentions = append(mentions, owner)
			}
		}
	}
	return mentions, nil
}

// ConfirmRuntimeCard decides exactly the immutable, accepted card snapshot.
// The caller must hold a current receiver lease for the authenticated app stream.
// Duplicate clicks revalidate current identity/scope but never decide new actions.
func (tx *Tx) ConfirmRuntimeCard(ctx context.Context, channelID string, cb RuntimeCardCallback) (string, error) {
	if cb.EventID == "" || !contains([]string{"agree", "reject", "confirm"}, cb.Action) || cb.UserIDType != 1 || (cb.SpaceType != "" && cb.SpaceType != "IM_GROUP") {
		return "", Fail("denied", "unsupported card decision callback")
	}
	decision := cb.Action
	if decision == "confirm" {
		decision = "agree"
	}
	app, err := ReadChannel(ctx, tx.Conn, channelID)
	if err != nil {
		return "", err
	}
	if app.Kind != ChannelDingTalkApp || app.Tenant != cb.CorpID || !contains([]string{"configured", "active"}, app.Status) {
		return "", Fail("denied", "card callback tenant or application is not admitted")
	}
	var taskID, content, digest, routeID, conversationID, format, state string
	err = tx.Conn.QueryRowContext(ctx, `SELECT job_id,content,input_digest,route_id,conversation_id,format,state FROM outbox WHERE id=? AND channel_id=?`, cb.CardID, channelID).Scan(&taskID, &content, &digest, &routeID, &conversationID, &format, &state)
	if err != nil {
		return "", Fail("denied", "card is not an application delivery")
	}
	scopeMatches := cb.SpaceID == "" || cb.SpaceID == conversationID
	if format == "confirmation_card" && state == "sending" && scopeMatches {
		return "", Fail("unavailable", "card delivery is still being recorded; callback must be redelivered")
	}
	if format != "confirmation_card" || state != "accepted" || !scopeMatches {
		return "", Fail("denied", "card is not accepted in its exact origin group")
	}
	var card RuntimeCard
	if json.Unmarshal([]byte(content), &card) != nil || len(card.Actions) == 0 {
		return "", Fail("conflict", "invalid card snapshot")
	}
	t, err := ReadRuntimeTask(ctx, tx.Conn, taskID)
	if err != nil {
		return "", err
	}
	c, err := ReadRuntime(ctx, tx.Conn, t.RuntimeID)
	if err != nil {
		return "", err
	}
	owner, err := runtimeCardOwner(ctx, tx.Conn, c)
	if err != nil {
		return "", err
	}
	if cb.UserID != owner.IDValue || c.ApplicationMode != "group_mention" || c.Status != "running" || c.ChannelID != channelID || t.RouteID != routeID {
		return "", Fail("denied", "only the authenticated DWS owner may decide the origin group card")
	}
	r, err := ReadRoute(ctx, tx.Conn, routeID)
	if err != nil {
		return "", err
	}
	admitted, err := runtimeTaskTriggerAdmitted(ctx, tx.Conn, c, t)
	if err != nil {
		return "", err
	}
	current, err := runtimeTaskMessagesCurrent(ctx, tx.Conn, t.ID)
	if err != nil {
		return "", err
	}
	if !admitted || !current || r.ConversationID != conversationID || r.ConversationType != "group" || r.Mode != "assistant" || r.SendPolicy != "reply_to_trigger" || !contains(r.Triggers, "mention") || t.Version != card.TaskVersion || card.TemplateID != app.Identity.ConfirmationCardTemplate || digest != Digest(map[string]any{"task": t.ID, "version": t.Version, "result": content}) {
		return "", Fail("conflict", "card, task, source or route changed")
	}
	seen := map[string]bool{}
	pending := []RuntimePendingAction{}
	for _, displayed := range card.Actions {
		if seen[displayed.ID] {
			return "", Fail("conflict", "duplicate card action")
		}
		seen[displayed.ID] = true
		found := false
		for _, a := range t.Actions {
			if a.ID != displayed.ID {
				continue
			}
			found = true
			if a.TaskVersion != t.Version || a.PayloadDigest != displayed.Digest || Hash([]byte(a.Payload)) != displayed.Digest || a.Kind != displayed.Kind || a.Target != displayed.Target {
				return "", Fail("conflict", "displayed action changed")
			}
			if a.Status == "pending" {
				pending = append(pending, a)
				continue
			}
			sameDecisionOrigin := a.ConfirmedBy == c.OwnerPrincipalID && strings.HasPrefix(a.ConfirmationOrigin, "dingtalk_card:"+cb.CardID+":")
			if !sameDecisionOrigin || (decision == "agree" && !contains([]string{"confirmed", "executing", "executed"}, a.Status)) || (decision == "reject" && a.Status != "rejected") {
				return "", Fail("conflict", "card action was already decided differently or elsewhere")
			}
		}
		if !found {
			return "", Fail("conflict", "displayed action no longer exists")
		}
	}
	if len(pending) == 0 {
		return decision, nil
	}
	if t.Status != "awaiting_confirmation" {
		return "", Fail("conflict", "task is no longer awaiting confirmation")
	}
	now := Now()
	origin := "dingtalk_card:" + cb.CardID + ":" + cb.EventID
	actionStatus := "confirmed"
	auditAction := "runtime.card.confirm"
	if decision == "reject" {
		actionStatus = "rejected"
		auditAction = "runtime.card.reject"
	}
	for _, a := range pending {
		res, err := tx.Conn.ExecContext(ctx, `UPDATE runtime_pending_actions SET status=?,confirmed_by=?,confirmation_origin=?,updated_at=? WHERE id=? AND status='pending'`, actionStatus, c.OwnerPrincipalID, origin, now, a.ID)
		if err != nil {
			return "", err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return "", Fail("conflict", "action was already handled")
		}
	}
	if decision == "reject" {
		res, updateErr := tx.Conn.ExecContext(ctx, `UPDATE runtime_tasks SET status='cancelled',error_code='owner_rejected',updated_at=? WHERE id=? AND version=? AND status='awaiting_confirmation'`, now, t.ID, t.Version)
		if updateErr != nil {
			return "", updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return "", Fail("conflict", "task was already handled")
		}
	}
	if _, err = tx.Audit(ctx, auditAction, origin, objectChange("outbox", cb.CardID)); err != nil {
		return "", err
	}
	return decision, nil
}
