package dingtalkapp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func callbackRaw(overrides map[string]any) []byte {
	data := map[string]any{"type": "actionCallback", "corpId": "corp1", "outTrackId": "outbox", "spaceId": "cid-group", "spaceType": "IM_GROUP", "userId": "owner", "userIdType": 1, "content": core.JSON(map[string]any{"cardPrivateData": map[string]any{"actionIds": []string{"confirm-button"}, "params": map[string]string{"action": "confirm"}}})}
	for k, v := range overrides {
		if v == nil {
			delete(data, k)
			continue
		}
		data[k] = v
	}
	return []byte(core.JSON(data))
}
func TestCardCallbackParsesOnlyPlatformActorAndDecisionParams(t *testing.T) {
	cb, err := ParseCardCallback(config(), "event", callbackRaw(nil))
	if err != nil || cb.CardID != "outbox" || cb.UserID != "owner" || cb.Action != "confirm" {
		t.Fatalf("callback=%+v err=%v", cb, err)
	}
	for _, action := range []string{"agree", "reject"} {
		raw := callbackRaw(map[string]any{"content": core.JSON(map[string]any{"cardPrivateData": map[string]any{"params": map[string]string{"action": action}}})})
		if cb, err = ParseCardCallback(config(), "event", raw); err != nil || cb.Action != action {
			t.Fatalf("action=%s callback=%+v err=%v", action, cb, err)
		}
	}
	// The official Stream contract omits these three optional metadata fields.
	// The accepted card snapshot remains bound by outTrackId in authoritative
	// storage, while the actor ID inherits the card's frozen userIdType=1.
	minimal := callbackRaw(map[string]any{"userIdType": nil, "spaceId": nil, "spaceType": nil})
	if cb, err = ParseCardCallback(config(), "minimal", minimal); err != nil || cb.UserIDType != 1 || cb.SpaceID != "" || cb.SpaceType != "" {
		t.Fatalf("documented minimal callback=%+v err=%v", cb, err)
	}
	prefixed := callbackRaw(map[string]any{"spaceId": "dtv1.card//IM_GROUP.cid-group"})
	if cb, err = ParseCardCallback(config(), "prefixed", prefixed); err != nil || cb.SpaceID != "cid-group" {
		t.Fatalf("prefixed callback=%+v err=%v", cb, err)
	}
	// DingTalk's card documentation and live callbacks use both upper- and
	// lower-case group space names, and examples exist with both protocol
	// separators. Normalize those equivalent group addresses before the
	// authoritative outbox/route comparison.
	for name, metadata := range map[string]map[string]any{
		"lower-case":      {"spaceId": "dtv1.card//im_group.cid-group", "spaceType": "im_group"},
		"colon-separator": {"spaceId": "dtv1.card://im_group.cid-group", "spaceType": "IM_GROUP"},
		"live-im-alias":   {"spaceId": "dtv1.card//im.cid-group", "spaceType": "im"},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, parseErr := ParseCardCallback(config(), name, callbackRaw(metadata))
			if parseErr != nil || parsed.SpaceID != "cid-group" || parsed.SpaceType != "IM_GROUP" {
				t.Fatalf("callback=%+v err=%v", parsed, parseErr)
			}
		})
	}
	spaceTypeOnly := callbackRaw(map[string]any{"spaceId": nil})
	if cb, err = ParseCardCallback(config(), "space-type-only", spaceTypeOnly); err != nil || cb.SpaceID != "" || cb.SpaceType != "IM_GROUP" {
		t.Fatalf("space-type-only callback=%+v err=%v", cb, err)
	}
	spaceIDOnly := callbackRaw(map[string]any{"spaceType": nil})
	if cb, err = ParseCardCallback(config(), "space-id-only", spaceIDOnly); err != nil || cb.SpaceID != "cid-group" || cb.SpaceType != "" {
		t.Fatalf("space-id-only callback=%+v err=%v", cb, err)
	}
	for _, override := range []map[string]any{{"type": "other"}, {"corpId": "other"}, {"userIdType": 2}, {"content": "not-json"}, {"content": `{"cardPrivateData":{"params":{"action":"accept","userId":"owner"}}}`}, {"spaceType": "IM_ROBOT"}, {"userId": ""}} {
		if _, err = ParseCardCallback(config(), "event", callbackRaw(override)); err == nil {
			t.Fatalf("unsafe callback=%+v", override)
		}
	}
}
func TestCardReceiverDoesNotUpdateCardForRejectedActorOrFailedCommit(t *testing.T) {
	for _, code := range []string{"", "denied", "unavailable"} {
		t.Run(code, func(t *testing.T) {
			confirmed, rejected := 0, 0
			a := &Adapter{Secrets: testSecret, Frames: func(ctx context.Context, cfg channel.Config, secret string, opts SessionOptions) error {
				result, err := opts.CardHandle(ctx, "click", callbackRaw(nil))
				if code == "unavailable" {
					if err == nil {
						t.Fatal("failed commit acknowledged")
					}
					return nil
				}
				if err != nil {
					t.Fatal(err)
				}
				raw := core.JSON(result)
				if code == "denied" && strings.Contains(raw, "agree") {
					t.Fatal("non-owner updated shared card")
				}
				if code == "" && !strings.Contains(raw, "agree") {
					t.Fatalf("missing status: %s", raw)
				}
				return nil
			}}
			err := a.RunReceiver(context.Background(), config(), channel.ReceiverOptions{Handle: func(context.Context, core.NormalizedEvent) error { return nil }, ConfirmCard: func(context.Context, core.RuntimeCardCallback) (string, error) {
				confirmed++
				if code != "" {
					return "", core.Fail(code, "test refusal")
				}
				return "agree", nil
			}, Reject: func(context.Context, channel.RejectedEvent) error { rejected++; return nil }})
			if err != nil || confirmed != 1 || (code == "denied" && rejected != 1) || (code == "unavailable" && rejected != 0) {
				t.Fatalf("confirmed=%d rejected=%d err=%v", confirmed, rejected, err)
			}
		})
	}
}
func TestCardSendUsesNativeMentionsAndStreamConfirmation(t *testing.T) {
	for _, format := range []string{"group_card", "confirmation_card"} {
		t.Run(format, func(t *testing.T) {
			cfg := config()
			cfg.Identity.ConfirmationCardTemplate = "confirm.schema"
			a, _ := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == accessTokenPath {
					_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "token", "expireIn": 7200})
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if format == "group_card" {
					if r.URL.Path != standardCardSendPath || body["cardTemplateId"] != "StandardCard" || body["openConversationId"] != "cid-group" || body["cardBizId"] != "outbox" {
						t.Fatalf("body=%+v path=%s", body, r.URL.Path)
					}
					var data map[string]any
					if json.Unmarshal([]byte(body["cardData"].(string)), &data) != nil {
						t.Fatalf("invalid StandardCard data: %+v", body["cardData"])
					}
					header := data["header"].(map[string]any)["title"].(map[string]any)
					contents := data["contents"].([]any)
					first := contents[0].(map[string]any)
					if header["text"] != "memgov 回复" || len(contents) != 1 || first["type"] != "markdown" || first["text"] != "full details" {
						t.Fatalf("StandardCard layout changed: data=%+v", data)
					}
					var mentions []map[string]string
					_ = json.Unmarshal([]byte(body["sendOptions"].(map[string]any)["atUserListJson"].(string)), &mentions)
					if len(mentions) != 2 || mentions[0]["userId"] != "requester" || mentions[1]["userId"] != "owner" {
						t.Fatalf("mentions=%+v", mentions)
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"processQueryKey": "receipt"})
				} else {
					if r.URL.Path != confirmationCardSendPath || body["callbackType"] != "STREAM" || body["openSpaceId"] != "dtv1.card//IM_GROUP.cid-group" || body["outTrackId"] != "outbox" {
						t.Fatalf("body=%+v", body)
					}
					mentions := body["imGroupOpenDeliverModel"].(map[string]any)["atUserIds"].(map[string]any)
					if len(mentions) != 2 || mentions["owner"] == nil || mentions["requester"] == nil {
						t.Fatalf("mentions=%+v", mentions)
					}
					params := body["cardData"].(map[string]any)["cardParamMap"].(map[string]any)
					if params["status"] != "pending" || params["title"] != "扩容 BuildHub 服务器" || params["summary"] != "待确认后执行 · 1 项操作" || params["details"] != "目标：buildhub\n内容：扩容至 8 核 16 GB" || params["reason"] != params["details"] || params["amount"] != "1 项" || params["type"] != "infra_change" || params["createTime"] == "" {
						t.Fatalf("params=%+v", params)
					}
					for _, key := range []string{"title", "summary", "details", "reason"} {
						if strings.ContainsAny(params[key].(string), "*`>") {
							t.Fatalf("markdown leaked through %s: %+v", key, params)
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"outTrackId": "outbox", "deliverResults": []map[string]any{{"success": true, "spaceId": "cid-group", "spaceType": "IM_GROUP", "carrierId": "receipt"}}}})
				}
			})
			card := core.RuntimeCard{Text: "full details", Mentions: []core.RuntimeCardMention{{IDType: "user_id", IDValue: "requester", Name: "Requester"}, {IDType: "user_id", IDValue: "owner", Name: "Owner"}}}
			if format == "confirmation_card" {
				card.TemplateID = "confirm.schema"
				card.Title = "**扩容 BuildHub 服务器**"
				card.Summary = "待确认后执行 · 1 项操作"
				card.Details = "> **目标：** buildhub\n- **内容：** `扩容至 8 核 16 GB`"
				card.Actions = []core.RuntimeCardAction{{ID: "action", Digest: "digest", Kind: "infra_change"}}
			}
			result, err := a.Send(context.Background(), cfg, channel.SendRequest{ConversationID: "cid-group", Transport: "bot_group", Format: format, Content: core.JSON(card), IdempotencyKey: "outbox"})
			if err != nil || result.State != "accepted" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
func TestConfirmationCardDoesNotAcceptIncompleteDeliveryResponse(t *testing.T) {
	a, _ := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == accessTokenPath {
			w.Write([]byte(`{"accessToken":"token","expireIn":7200}`))
			return
		}
		w.Write([]byte(`{"success":true,"result":{"outTrackId":"outbox","deliverResults":[{"success":true,"spaceId":"another-group","spaceType":"IM_GROUP","carrierId":"receipt"}]}}`))
	})
	cfg := config()
	cfg.Identity.ConfirmationCardTemplate = "confirm.schema"
	result, err := a.Send(context.Background(), cfg, channel.SendRequest{ConversationID: "cid-group", Transport: "bot_group", Format: "confirmation_card", Content: core.JSON(core.RuntimeCard{Text: "details", TemplateID: "confirm.schema", Actions: []core.RuntimeCardAction{{ID: "a"}}}), IdempotencyKey: "outbox"})
	if err != nil || result.State != "unknown" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
