package dingtalkapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdkcard "github.com/open-dingtalk/dingtalk-stream-sdk-go/card"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

const standardCardSendPath = "/v1.0/im/v1.0/robot/interactiveCards/send"
const confirmationCardSendPath = "/v1.0/card/instances/createAndDeliver"

func ParseCardCallback(cfg channel.Config, eventID string, raw []byte) (core.RuntimeCardCallback, error) {
	var cb core.RuntimeCardCallback
	if len(raw) > 64<<10 || eventID == "" || len(eventID) > 512 {
		return cb, core.Fail("invalid_input", "invalid card callback size or event identifier")
	}
	var request sdkcard.CardRequest
	if json.Unmarshal(raw, &request) != nil {
		return cb, core.Fail("invalid_input", "invalid card callback JSON")
	}
	if request.CorpId != cfg.Tenant || request.CorpId != cfg.Identity.ExpectedCorpID {
		return cb, core.Fail("denied", "card callback belongs to another enterprise")
	}
	if request.Type != "actionCallback" {
		return cb, core.Fail("denied", "unsupported card callback type")
	}
	var data sdkcard.PrivateCardActionData
	if json.Unmarshal([]byte(request.Content), &data) != nil {
		return cb, core.Fail("invalid_input", "invalid private card action data")
	}
	action, ok := data.CardPrivateData.Params["action"].(string)
	if !ok || !containsCardDecision(action) {
		return cb, core.Fail("denied", "unsupported card decision action")
	}
	if request.OutTrackId == "" || len(request.OutTrackId) > 512 || request.UserId == "" || len(request.UserId) > 512 {
		return cb, core.Fail("denied", "invalid card or actor identifier")
	}
	// DingTalk's documented Stream callback contains the authenticated corpId,
	// userId and outTrackId, but may omit userIdType and group-space metadata.
	// This application creates confirmation cards only with userIdType=1, so an
	// omitted zero value inherits that frozen delivery contract. A conflicting
	// explicit type is still rejected.
	if request.UserIdType != 0 && request.UserIdType != 1 {
		return cb, core.Fail("denied", "card callback actor uses an unexpected identifier type")
	}
	request.UserIdType = 1
	if request.SpaceType != "" {
		if !cardGroupSpaceType(request.SpaceType) {
			return cb, core.Fail("denied", "card callback has invalid group space type %q", request.SpaceType)
		}
		request.SpaceType = "IM_GROUP"
	}
	if request.SpaceId != "" {
		if len(request.SpaceId) > 512 {
			return cb, core.Fail("denied", "card callback group space identifier exceeds the length limit")
		}
		request.SpaceId = normalizeCardGroupSpaceID(request.SpaceId)
		if request.SpaceId == "" {
			return cb, core.Fail("denied", "card callback has an empty group space identifier")
		}
	}
	cb = core.RuntimeCardCallback{EventID: eventID, CorpID: request.CorpId, CardID: request.OutTrackId, SpaceID: request.SpaceId, SpaceType: request.SpaceType, UserID: request.UserId, UserIDType: request.UserIdType, Action: action}
	return cb, nil
}

func cardGroupSpaceType(value string) bool {
	return strings.EqualFold(value, "IM_GROUP") || strings.EqualFold(value, "IM")
}

func normalizeCardGroupSpaceID(value string) string {
	for _, prefix := range []string{"dtv1.card//IM_GROUP.", "dtv1.card://IM_GROUP.", "dtv1.card//IM.", "dtv1.card://IM."} {
		if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			return value[len(prefix):]
		}
	}
	return value
}

func containsCardDecision(action string) bool {
	return action == "agree" || action == "reject" || action == "confirm"
}
func containsCardRefusal(code string) bool {
	return code == "denied" || code == "conflict" || code == "invalid_input" || code == "not_found"
}
func cardStatusResponse(status string) any {
	return map[string]any{"cardUpdateOptions": map[string]bool{"updateCardDataByKey": true}, "cardData": map[string]any{"cardParamMap": map[string]string{"status": status}}}
}

func (a *Adapter) sendCard(ctx context.Context, cfg channel.Config, req channel.SendRequest) (channel.SendResult, error) {
	var card core.RuntimeCard
	if req.Transport != "bot_group" || req.IdempotencyKey == "" || json.Unmarshal([]byte(req.Content), &card) != nil || strings.TrimSpace(card.Text) == "" {
		return channel.SendResult{State: "failed"}, core.Fail("denied", "card requires a frozen group delivery and outbox identifier")
	}
	if req.Format == "confirmation_card" {
		if card.TemplateID == "" || card.TemplateID != cfg.Identity.ConfirmationCardTemplate || !strings.HasSuffix(card.TemplateID, ".schema") || len(card.Actions) == 0 {
			return channel.SendResult{State: "failed"}, core.Fail("denied", "confirmation card template is not configured")
		}
		atUsers := map[string]string{}
		for _, m := range card.Mentions {
			if m.IDType == "user_id" {
				atUsers[m.IDValue] = m.Name
			}
		}
		// The shared template contract contains no authorization IDs. A click
		// decides the server's snapshot after its own actor/scope checks.
		created := a.clock().Format("2006-01-02 15:04:05")
		title := truncateCardText(plainCardText(card.Title), 48)
		if title == "" {
			title = "待确认操作"
		}
		summary := truncateCardText(plainCardText(card.Summary), 96)
		if summary == "" {
			summary = fmt.Sprintf("待确认后执行 · %d 项操作", len(card.Actions))
		}
		details := truncateCardText(plainCardText(card.Details), 1200)
		if details == "" {
			details = truncateCardText(plainCardText(card.Text), 1200)
		}
		actionType := truncateCardText(cardActionTypes(card.Actions), 80)
		params := map[string]string{
			"title": title, "summary": summary, "details": details, "status": "pending",
			// The published DingTalk approval preset uses these variable names.
			// Keep the generic fields above so a later template can be swapped
			// without changing the authorization callback contract.
			"lastMessage": summary, "type": actionType, "amount": fmt.Sprintf("%d 项", len(card.Actions)), "reason": details, "createTime": created,
		}
		raw, err := a.post(ctx, cfg, confirmationCardSendPath, map[string]any{
			"cardTemplateId": card.TemplateID, "outTrackId": req.IdempotencyKey, "userIdType": 1, "callbackType": "STREAM",
			"cardData":                map[string]any{"cardParamMap": params},
			"openSpaceId":             "dtv1.card//IM_GROUP." + req.ConversationID,
			"imGroupOpenSpaceModel":   map[string]bool{"supportForward": false},
			"imGroupOpenDeliverModel": map[string]any{"robotCode": cfg.Identity.RobotCode, "atUserIds": atUsers},
		})
		if err != nil {
			return channel.SendResult{State: "unknown"}, err
		}
		var response struct {
			Success bool `json:"success"`
			Result  struct {
				OutTrackID     string `json:"outTrackId"`
				DeliverResults []struct {
					Success   bool   `json:"success"`
					SpaceID   string `json:"spaceId"`
					SpaceType string `json:"spaceType"`
					CarrierID string `json:"carrierId"`
				} `json:"deliverResults"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &response) != nil || !response.Success || response.Result.OutTrackID != req.IdempotencyKey {
			return channel.SendResult{State: "unknown"}, nil
		}
		for _, delivered := range response.Result.DeliverResults {
			if delivered.SpaceID == req.ConversationID && delivered.SpaceType == "IM_GROUP" && delivered.Success && delivered.CarrierID != "" {
				return channel.SendResult{State: "accepted", Receipt: core.Hash([]byte(delivered.CarrierID))}, nil
			}
		}
		return channel.SendResult{State: "unknown"}, nil
	}
	mentions := []map[string]string{}
	for _, m := range card.Mentions {
		item := map[string]string{"nickName": m.Name}
		if m.IDType == "user_id" {
			item["userId"] = m.IDValue
		} else if m.IDType == "union_id" {
			item["unionId"] = m.IDValue
		} else {
			continue
		}
		mentions = append(mentions, item)
	}
	data := map[string]any{"config": map[string]bool{"autoLayout": true, "enableForward": false}, "header": map[string]any{"title": map[string]string{"type": "text", "text": "memgov 回复"}}, "contents": []map[string]string{{"type": "markdown", "text": card.Text, "id": "details"}}}
	raw, err := a.post(ctx, cfg, standardCardSendPath, map[string]any{"robotCode": cfg.Identity.RobotCode, "openConversationId": req.ConversationID, "cardTemplateId": "StandardCard", "cardBizId": req.IdempotencyKey, "cardData": core.JSON(data), "pullStrategy": false, "sendOptions": map[string]any{"atAll": false, "atUserListJson": core.JSON(mentions)}})
	if err != nil {
		return channel.SendResult{State: "unknown"}, err
	}
	var response struct {
		ProcessQueryKey string `json:"processQueryKey"`
	}
	if json.Unmarshal(raw, &response) != nil || response.ProcessQueryKey == "" {
		return channel.SendResult{State: "unknown"}, nil
	}
	return channel.SendResult{State: "accepted", Receipt: core.Hash([]byte(response.ProcessQueryKey))}, nil
}

func plainCardText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.NewReplacer("**", "", "__", "", "`", "").Replace(value)
	lines := make([]string, 0, strings.Count(value, "\n")+1)
	for _, raw := range strings.Split(value, "\n") {
		line := strings.TrimSpace(raw)
		for _, prefix := range []string{"### ", "## ", "# ", "> ", "- ", "* "} {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				break
			}
		}
		if line == "" || line == "---" {
			continue
		}
		line = strings.Join(strings.Fields(line), " ")
		line = strings.ReplaceAll(line, "： ", "：")
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func truncateCardText(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func cardActionTypes(actions []core.RuntimeCardAction) string {
	seen := map[string]bool{}
	types := make([]string, 0, len(actions))
	for _, action := range actions {
		kind := plainCardText(action.Kind)
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		types = append(types, kind)
	}
	if len(types) == 0 {
		return "受控操作"
	}
	return strings.Join(types, "、")
}
