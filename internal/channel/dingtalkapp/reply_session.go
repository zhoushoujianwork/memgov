package dingtalkapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

const maxReplySessions = 2048

type replySessionKey struct {
	channelID      string
	conversationID string
	messageID      string
}

type replySession struct {
	webhook    string
	expiresAt  time.Time
	senderID   string
	capturedAt time.Time
}

// rememberReplySession extracts only the short-lived reply capability. The
// normalized event remains the source of identity and routing truth.
func (a *Adapter) rememberReplySession(cfg channel.Config, event core.NormalizedEvent, raw []byte) {
	if event.ConversationType != "group" || event.Sender.IDType != "user_id" || event.Sender.IDValue == "" {
		return
	}
	var callback struct {
		ConversationID            string `json:"conversationId"`
		MsgID                     string `json:"msgId"`
		SenderStaffID             string `json:"senderStaffId"`
		SessionWebhook            string `json:"sessionWebhook"`
		SessionWebhookExpiredTime int64  `json:"sessionWebhookExpiredTime"`
	}
	if json.Unmarshal(raw, &callback) != nil || callback.ConversationID != event.ConversationID || callback.MsgID != event.ProviderMessageID || callback.SenderStaffID != event.Sender.IDValue {
		return
	}
	expires := time.UnixMilli(callback.SessionWebhookExpiredTime)
	if !expires.After(a.clock()) || !validSessionWebhook(callback.SessionWebhook) {
		return
	}
	key := replySessionKey{channelID: cfg.ChannelID, conversationID: event.ConversationID, messageID: event.ProviderMessageID}
	a.replyMu.Lock()
	defer a.replyMu.Unlock()
	if a.replySessions == nil {
		a.replySessions = map[replySessionKey]replySession{}
	}
	if len(a.replySessions) >= maxReplySessions {
		for candidate, session := range a.replySessions {
			if !session.expiresAt.After(a.clock()) {
				delete(a.replySessions, candidate)
			}
		}
	}
	if len(a.replySessions) >= maxReplySessions {
		var oldestKey replySessionKey
		var oldest time.Time
		for candidate, session := range a.replySessions {
			if oldest.IsZero() || session.capturedAt.Before(oldest) {
				oldestKey, oldest = candidate, session.capturedAt
			}
		}
		delete(a.replySessions, oldestKey)
	}
	a.replySessions[key] = replySession{webhook: callback.SessionWebhook, expiresAt: expires, senderID: callback.SenderStaffID, capturedAt: a.clock()}
}

func validSessionWebhook(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "oapi.dingtalk.com") && u.User == nil && u.Fragment == "" && u.Path == "/robot/sendBySession" && u.RawQuery != ""
}

func (a *Adapter) takeReplySession(cfg channel.Config, req channel.SendRequest, mention core.RuntimeCardMention) (replySession, bool) {
	key := replySessionKey{channelID: cfg.ChannelID, conversationID: req.ConversationID, messageID: req.ReplyTo}
	a.replyMu.Lock()
	defer a.replyMu.Unlock()
	session, ok := a.replySessions[key]
	if ok {
		delete(a.replySessions, key)
	}
	if !ok || !session.expiresAt.After(a.clock()) || mention.IDType != "user_id" || mention.IDValue != session.senderID {
		return replySession{}, false
	}
	return session, true
}

func nativeMentionText(text string, mention core.RuntimeCardMention) string {
	text = strings.TrimSpace(text)
	name := strings.Join(strings.Fields(mention.Name), " ")
	if name != "" {
		suffix := "\n\n@" + name
		if strings.HasSuffix(text, suffix) {
			text = strings.TrimSuffix(text, suffix)
		}
	}
	return strings.TrimSpace(text) + "\n\n@" + mention.IDValue
}

// sendSessionMarkdown uses the callback-scoped endpoint that actually supports
// native @. handled=false means no usable callback capability existed and the
// caller may safely use the proactive endpoint as a plain-text fallback.
func (a *Adapter) sendSessionMarkdown(ctx context.Context, cfg channel.Config, req channel.SendRequest, message core.RuntimeCard) (result channel.SendResult, handled bool, err error) {
	if len(message.Mentions) != 1 {
		return channel.SendResult{}, false, nil
	}
	session, ok := a.takeReplySession(cfg, req, message.Mentions[0])
	if !ok {
		return channel.SendResult{}, false, nil
	}
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"title": "memgov AI 值守", "text": nativeMentionText(message.Text, message.Mentions[0])},
		"at":       map[string]any{"atUserIds": []string{session.senderID}},
	}
	body, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return channel.SendResult{State: "failed"}, true, core.Fail("invalid_input", "DingTalk reply could not be encoded")
	}
	httpReq, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, session.webhook, bytes.NewReader(body))
	if requestErr != nil {
		return channel.SendResult{State: "failed"}, true, core.Fail("invalid_input", "DingTalk reply webhook is invalid")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := *a.client()
	// A callback credential is valid only for the verified DingTalk endpoint.
	// Never carry its signed query through an HTTP redirect to another host.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, requestErr := client.Do(httpReq)
	if requestErr != nil {
		return channel.SendResult{State: "unknown"}, true, core.Fail("unavailable", "DingTalk session reply outcome is unknown")
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return channel.SendResult{State: "unknown"}, true, core.Fail("unavailable", "DingTalk session reply response could not be read")
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// The platform definitely rejected this expired/invalid reply capability;
		// a proactive plain Markdown fallback cannot duplicate an accepted reply.
		return channel.SendResult{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return channel.SendResult{State: "unknown"}, true, core.Fail("unavailable", "DingTalk session reply returned status %d", resp.StatusCode)
	}
	var out struct {
		ErrorCode int    `json:"errcode"`
		ErrorMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return channel.SendResult{State: "unknown"}, true, nil
	}
	if out.ErrorCode != 0 {
		return channel.SendResult{}, false, nil
	}
	return channel.SendResult{State: "accepted", Receipt: core.Hash([]byte(req.IdempotencyKey + ":session_reply"))}, true, nil
}
