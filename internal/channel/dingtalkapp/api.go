package dingtalkapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

const (
	accessTokenPath = "/v1.0/oauth2/accessToken"
	directSendPath  = "/v1.0/robot/oToMessages/batchSend"
	groupSendPath   = "/v1.0/robot/groupMessages/send"
	reactionAddPath = "/v1.0/robot/emotion/reply"
	reactionDelPath = "/v1.0/robot/emotion/recall"
)

func (a *Adapter) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (a *Adapter) base() string {
	if strings.TrimSpace(a.APIBase) != "" {
		return strings.TrimRight(a.APIBase, "/")
	}
	return "https://api.dingtalk.com"
}

func (a *Adapter) clock() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func secretFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (a *Adapter) accessToken(ctx context.Context, cfg channel.Config, secret string) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	fingerprint := secretFingerprint(secret)
	if a.token != "" && a.tokenClient == cfg.Identity.ClientID && a.tokenSecret == fingerprint && a.clock().Before(a.tokenExpiry) {
		return a.token, nil
	}
	body, _ := json.Marshal(map[string]string{"appKey": cfg.Identity.ClientID, "appSecret": secret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+accessTokenPath, bytes.NewReader(body))
	if err != nil {
		return "", core.Fail("unavailable", "could not create DingTalk token request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return "", core.Fail("unavailable", "DingTalk token request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", core.Fail("denied", "DingTalk rejected the application credential with status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || out.AccessToken == "" || len(out.AccessToken) > 4096 {
		return "", core.Fail("unavailable", "DingTalk returned an invalid access token response")
	}
	ttl := time.Duration(out.ExpireIn) * time.Second
	if ttl <= 0 || ttl > 2*time.Hour {
		ttl = 2 * time.Hour
	}
	if ttl > 5*time.Minute {
		ttl -= 5 * time.Minute
	}
	a.token, a.tokenClient, a.tokenSecret, a.tokenExpiry = out.AccessToken, cfg.Identity.ClientID, fingerprint, a.clock().Add(ttl)
	return a.token, nil
}

func (a *Adapter) post(ctx context.Context, cfg channel.Config, path string, payload any) ([]byte, error) {
	secret, err := a.secret(ctx, cfg)
	if err != nil {
		return nil, err
	}
	token, err := a.accessToken(ctx, cfg, secret)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, core.Fail("invalid_input", "DingTalk request could not be encoded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+path, bytes.NewReader(body))
	if err != nil {
		return nil, core.Fail("unavailable", "could not create DingTalk request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", token)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, core.Fail("unavailable", "DingTalk request outcome is unknown")
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return nil, core.Fail("unavailable", "DingTalk response could not be read")
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			a.tokenMu.Lock()
			a.token = ""
			a.tokenMu.Unlock()
			return nil, core.Fail("denied", "DingTalk rejected the robot operation with status %d", resp.StatusCode)
		}
		return nil, core.Fail("unavailable", "DingTalk robot operation returned status %d", resp.StatusCode)
	}
	return raw, nil
}

func (a *Adapter) Send(ctx context.Context, cfg channel.Config, req channel.SendRequest) (channel.SendResult, error) {
	if strings.TrimSpace(req.ConversationID) == "" || strings.TrimSpace(req.Content) == "" {
		return channel.SendResult{State: "blocked"}, core.Fail("invalid_input", "bot delivery requires a conversation and content")
	}
	if req.Format == "group_card" || req.Format == "confirmation_card" {
		return a.sendCard(ctx, cfg, req)
	}
	var groupMessage core.RuntimeCard
	if req.Format == "group_markdown" {
		if req.Transport != "bot_group" || req.IdempotencyKey == "" || req.ReplyTo == "" || json.Unmarshal([]byte(req.Content), &groupMessage) != nil || strings.TrimSpace(groupMessage.Text) == "" {
			return channel.SendResult{State: "failed"}, core.Fail("denied", "group markdown requires a frozen group delivery and outbox identifier")
		}
		for _, mention := range groupMessage.Mentions {
			if mention.IDType != "user_id" && mention.IDType != "union_id" {
				return channel.SendResult{State: "failed"}, core.Fail("denied", "group markdown contains an unsupported mention identity")
			}
		}
		if result, handled, err := a.sendSessionMarkdown(ctx, cfg, req, groupMessage); handled {
			return result, err
		}
		req.Content = groupMessage.Text
	}
	param, _ := json.Marshal(map[string]string{"title": "memgov AI 值守", "text": req.Content})
	if req.Transport == "bot_group" {
		payload := map[string]any{"robotCode": cfg.Identity.RobotCode, "openConversationId": req.ConversationID, "msgKey": "sampleMarkdown", "msgParam": string(param)}
		raw, err := a.post(ctx, cfg, groupSendPath, payload)
		if err != nil {
			return channel.SendResult{State: "unknown"}, err
		}
		var out struct {
			ProcessQueryKey string `json:"processQueryKey"`
		}
		if json.Unmarshal(raw, &out) != nil || out.ProcessQueryKey == "" {
			return channel.SendResult{State: "unknown"}, nil
		}
		return channel.SendResult{State: "accepted", Receipt: core.Hash([]byte(out.ProcessQueryKey))}, nil
	}
	if req.Transport != "bot_dm" {
		return channel.SendResult{State: "blocked"}, core.Fail("invalid_input", "unsupported DingTalk bot transport")
	}
	raw, err := a.post(ctx, cfg, directSendPath, map[string]any{"robotCode": cfg.Identity.RobotCode, "userIds": []string{req.ConversationID}, "msgKey": "sampleMarkdown", "msgParam": string(param)})
	if err != nil {
		return channel.SendResult{State: "unknown"}, err
	}
	var out struct {
		ProcessQueryKey string   `json:"processQueryKey"`
		Invalid         []string `json:"invalidStaffIdList"`
		Filtered        []string `json:"filteredStaffIdList"`
		FlowControlled  []string `json:"flowControlledStaffIdList"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return channel.SendResult{State: "unknown"}, nil
	}
	if len(out.Invalid)+len(out.Filtered)+len(out.FlowControlled) > 0 {
		return channel.SendResult{State: "failed"}, core.Fail("denied", "DingTalk did not accept the configured owner as a delivery target")
	}
	if out.ProcessQueryKey == "" {
		return channel.SendResult{State: "unknown"}, nil
	}
	return channel.SendResult{State: "accepted", Receipt: core.Hash([]byte(out.ProcessQueryKey))}, nil
}

func (a *Adapter) AddReaction(ctx context.Context, cfg channel.Config, req channel.ReactionRequest) error {
	return a.reaction(ctx, cfg, reactionAddPath, req)
}

func (a *Adapter) RemoveReaction(ctx context.Context, cfg channel.Config, req channel.ReactionRequest) error {
	return a.reaction(ctx, cfg, reactionDelPath, req)
}

func (a *Adapter) reaction(ctx context.Context, cfg channel.Config, path string, req channel.ReactionRequest) error {
	if strings.TrimSpace(req.ConversationID) == "" || strings.TrimSpace(req.MessageID) == "" || strings.TrimSpace(req.Emoji) == "" {
		return core.Fail("invalid_input", "reaction requires conversation, message and emoji")
	}
	raw, err := a.post(ctx, cfg, path, map[string]any{"robotCode": cfg.Identity.RobotCode, "openConversationId": req.ConversationID, "openMsgId": req.MessageID, "emotionType": 2, "emotionName": req.Emoji, "textEmotion": map[string]any{"emotionId": "2659900", "emotionName": req.Emoji, "text": req.Emoji, "backgroundId": "im_bg_1"}})
	if err != nil {
		return err
	}
	var out struct {
		Success bool `json:"success"`
	}
	if json.Unmarshal(raw, &out) != nil || !out.Success {
		return core.Fail("unavailable", "DingTalk did not confirm the %s reaction", strings.TrimPrefix(path, "/v1.0/robot/emotion/"))
	}
	return nil
}
