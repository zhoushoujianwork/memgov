package runtime

import (
	"context"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// executeConfirmedBotForward uses the frozen, Owner-confirmed action. The
// adapter's application credentials, not any DWS personal profile, send it.
func (s *Service) executeConfirmedBotForward(ctx context.Context, cfg core.RuntimeConfig, task core.RuntimeTask, action core.RuntimePendingAction) (core.RuntimeAttemptResult, bool, error) {
	var output core.RuntimeAttemptResult
	if cfg.ApplicationMode != "group_mention" || cfg.ExternalActions != "owner_confirmation" || action.Kind != core.RuntimeBotForwardAction || action.ConfirmedBy != cfg.OwnerPrincipalID || !(strings.HasPrefix(action.ConfirmationOrigin, "dingtalk_message:") || strings.HasPrefix(action.ConfirmationOrigin, "dingtalk_card:")) || action.TaskID != task.ID || action.TaskVersion != task.Version {
		return output, false, core.Fail("denied", "bot forwarding is not the confirmed action")
	}
	payload, err := core.ParseRuntimeBotForward(action)
	if err != nil {
		return output, false, err
	}
	app, err := core.ReadChannel(ctx, s.Store.DB, cfg.ChannelID)
	if err != nil {
		return output, false, err
	}
	if app.Kind != core.ChannelDingTalkApp || !app.Capabilities.Verified["send"] || app.Status != "active" && app.Status != "configured" {
		return output, false, core.Fail("denied", "application bot send is unavailable")
	}
	history, err := core.ReadChannel(ctx, s.Store.DB, app.Identity.HistoryChannel)
	if err != nil || history.Kind != core.ChannelDwsPersonal || history.Tenant != app.Tenant || history.Identity.Profile == "" {
		return output, false, core.Fail("denied", "bot forwarding directory binding changed")
	}
	targetType, targetID, ok := strings.Cut(action.Target, ":")
	if !ok || targetID == "" {
		return output, false, core.Fail("invalid_input", "confirmed bot message target or content is invalid")
	}
	transport := "bot_dm"
	switch targetType {
	case "user":
		// The pre-confirmation MCP resolved this stable ID through the bound
		// same-tenant directory; confirmation froze the exact target and body.
	case "group":
		route, routeErr := core.RouteFor(ctx, s.Store.DB, app.ID, targetID)
		if routeErr != nil || route.Status != "active" || route.ConversationType != "group" || route.Mode != "assistant" {
			return output, false, core.Fail("denied", "target group is no longer mounted on this bot")
		}
		mounted := false
		for _, id := range cfg.RouteIDs {
			mounted = mounted || id == route.ID
		}
		if !mounted {
			return output, false, core.Fail("denied", "target group is outside the current bot runtime")
		}
		transport = "bot_group"
	default:
		return output, false, core.Fail("invalid_input", "confirmed bot message target type is invalid")
	}
	result, sendErr := s.Adapter.Send(ctx, channel.ConfigFor(app), channel.SendRequest{ConversationID: targetID, Transport: transport, Content: payload.Content, Format: "markdown", IdempotencyKey: action.ID})
	if sendErr != nil {
		return output, result.State != "failed" && result.State != "blocked", sendErr
	}
	if result.State != "accepted" {
		return output, result.State != "failed" && result.State != "blocked", core.Fail("unavailable", "DingTalk bot message was not accepted")
	}
	output.Result = "DingTalk accepted the confirmed application-bot message to " + action.Target + "."
	output.Summary = "Confirmed bot message accepted by DingTalk"
	output.ToolKinds = []string{"bot_message_send"}
	return output, false, nil
}
