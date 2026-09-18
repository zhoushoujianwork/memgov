package core

import (
	"context"
	"encoding/json"
	"strings"
)

const RuntimeDestructiveAction = "destructive_operation"

func validateDestructiveAction(a RuntimeAction) error {
	var proposal struct{ Operation, Impact, Recovery string }
	if json.Unmarshal([]byte(a.Payload), &proposal) != nil || strings.TrimSpace(proposal.Operation) == "" || strings.TrimSpace(proposal.Impact) == "" || strings.TrimSpace(proposal.Recovery) == "" {
		return Fail("invalid_input", "destructive proposal requires concrete operation, impact and recovery")
	}
	return nil
}

// Approval remains tied to the live, verified identity and exact evidence. A
// revoked alias or recalled approval cannot be used by an already queued job.
func destructiveApprovalCurrent(ctx context.Context, q Queryer, a RuntimePendingAction) error {
	if a.Kind != RuntimeDestructiveAction {
		return nil
	}
	t, err := ReadRuntimeTask(ctx, q, a.TaskID)
	if err != nil {
		return err
	}
	c, err := ReadRuntime(ctx, q, t.RuntimeID)
	if err != nil {
		return err
	}
	if c.ApplicationMode != "proactive" {
		return nil
	}
	messageID := strings.TrimPrefix(a.ConfirmationOrigin, "dingtalk_message:")
	if messageID == a.ConfirmationOrigin {
		return Fail("denied", "destructive operation requires live Owner approval")
	}
	var channel, conversation, body string
	err = q.QueryRowContext(ctx, `SELECT m.channel_id,m.conversation_id,mr.body FROM messages m JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision WHERE m.id=? AND m.availability='available' AND m.context_only=0`, messageID).Scan(&channel, &conversation, &body)
	if err != nil || strings.TrimSpace(body) != ConfirmationToken(a) {
		return Fail("denied", "Owner approval changed or is unavailable")
	}
	verified, err := runtimeConfirmationSenderCurrent(ctx, q, channel, messageID, c.OwnerPrincipalID)
	if err != nil {
		return err
	}
	if !verified {
		return Fail("denied", "Owner approval identity is no longer verified")
	}
	routes, err := runtimeConfirmationRoutes(ctx, q, c)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if r.ChannelID == channel && r.ConversationID == conversation {
			return nil
		}
	}
	return Fail("denied", "Owner approval route is no longer authorized")
}
