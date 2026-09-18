// Package dingtalkapp receives application-bot messages over the official
// DingTalk Stream connection. It is a separate channel kind from the personal
// dws adapter: it uses the application's own identity and permissions, and the
// two are never substituted for each other.
package dingtalkapp

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// ParseVersion travels with every event, so a parser change is visible in the
// stored record rather than silently altering history.
const ParseVersion = "dingtalk_app/6"

// maxFrameBytes bounds one callback payload. A larger frame is isolated instead
// of parsed, because a truncated payload cannot be trusted.
const maxFrameBytes = 1 << 20

// Body limits apply to ordinary text and normalized richText. Media remains
// unreadable until a separate, authorized retrieval path is implemented.
const maxBodyBytes = 64 << 10
const maxRichTextParts = 128

// botCallback is the subset of the bot callback we actually rely on. Fields the
// adapter does not use are deliberately absent so they never reach the database.
type botCallback struct {
	ConversationID   string `json:"conversationId"`
	ConversationType string `json:"conversationType"`
	MsgID            string `json:"msgId"`
	MsgType          string `json:"msgtype"`
	SenderID         string `json:"senderId"`
	SenderStaffID    string `json:"senderStaffId"`
	SenderNick       string `json:"senderNick"`
	SenderCorpID     string `json:"senderCorpId"`
	ChatbotCorpID    string `json:"chatbotCorpId"`
	ChatbotUserID    string `json:"chatbotUserId"`
	RobotCode        string `json:"robotCode"`
	CreateAt         int64  `json:"createAt"`
	IsInAtList       bool   `json:"isInAtList"`
	OriginalMsgID    string `json:"originalMsgId"`
	Text             struct {
		Content    string          `json:"content"`
		IsReplyMsg bool            `json:"isReplyMsg"`
		RepliedMsg *quotedCallback `json:"repliedMsg"`
	} `json:"text"`
	Content json.RawMessage `json:"content"`
}

// secretFields are stripped before a payload is stored. A session webhook is a
// bearer credential: it authorizes sending to that conversation, so it must not
// reach SQLite, a log, a model context or an export.
var secretFields = []string{"sessionWebhook", "sessionWebhookExpiredTime", "robotCode", "chatbotUserId",
	"downloadCode", "pictureDownloadCode", "mediaId", "fileId", "url"}

// redact removes credentials and expiry hints from the raw frame while keeping
// the rest as evidence of what actually arrived.
func redact(raw []byte) string {
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		// An unparseable payload is kept verbatim only if it cannot contain a
		// webhook; otherwise the whole payload is dropped rather than risked.
		if containsSecretField(string(raw)) {
			return `{"redacted":"payload withheld because it could not be parsed and may contain a credential"}`
		}
		return string(raw)
	}
	return core.JSON(redactFields(generic))
}

func containsSecretField(raw string) bool {
	for _, field := range secretFields {
		if strings.Contains(raw, field) {
			return true
		}
	}
	return false
}

func redactFields(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			secret := false
			for _, field := range secretFields {
				if key == field {
					secret = true
					break
				}
			}
			if secret {
				typed[key] = "[redacted]"
			} else {
				typed[key] = redactFields(item)
			}
		}
	case []any:
		for i, item := range typed {
			typed[i] = redactFields(item)
		}
	case string:
		// Some quoted content is a JSON-encoded object inside a JSON string.
		// Walk that layer too so media capabilities cannot survive in evidence.
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var nested any
			if json.Unmarshal([]byte(trimmed), &nested) == nil {
				return core.JSON(redactFields(nested))
			}
		}
	}
	return value
}

// ParseFrame turns one bot callback into a normalized event. It refuses a frame
// from another tenant or another robot, so a misdirected subscription cannot
// write into this channel's history.
func ParseFrame(cfg channel.Config, raw []byte) (core.NormalizedEvent, error) {
	var out core.NormalizedEvent
	if len(raw) > maxFrameBytes {
		return out, core.Fail("invalid_input", "callback payload exceeds the %d byte limit", maxFrameBytes)
	}
	var cb botCallback
	if err := json.Unmarshal(raw, &cb); err != nil {
		return out, core.Fail("invalid_input", "callback payload is not valid JSON: %v", err)
	}
	// The tenant is checked against the configured one before anything else. The
	// bot's own corp ID is the authoritative one for the connection; the sender's
	// may legitimately differ for an external contact, so it is not used here.
	if cfg.Tenant != "" && cb.ChatbotCorpID != cfg.Tenant {
		return out, core.Fail("denied", "callback corp does not match the bound enterprise")
	}
	// chatbotUserId is a DingTalk user identifier (often prefixed with "$:").
	// The outbound API robotCode is a different namespace; the Stream callback
	// carries that code separately. Require its exact match so a second bot under
	// the same application cannot be imported into this channel when the field
	// is present. The SDK callback model omits robotCode, so an absent field is
	// not evidence of a foreign bot; the authenticated app Stream and bound route
	// remain the ingestion boundary in that case.
	if cfg.Identity.RobotCode != "" && cb.RobotCode != "" && cb.RobotCode != cfg.Identity.RobotCode {
		return out, core.Fail("denied", "callback robot code does not match the bound application bot")
	}
	body, attachments, bodyErr := callbackBody(cb)
	if bodyErr != nil {
		return out, bodyErr
	}
	if cb.MsgID == "" || cb.ConversationID == "" {
		return out, core.Fail("invalid_input", "callback is missing msgId or conversationId")
	}
	// DingTalk's Stream bot tutorial sends SenderStaffId as the private-chat
	// receiver's userId. Keep this exact enterprise identifier typed as user_id;
	// this parser change does not relabel historical staff_id principals.
	// https://open-dingtalk.github.io/developerpedia/docs/explore/tutorials/stream/bot/go/send-streaming-card/
	sender := core.Sender{IDType: "user_id", IDValue: cb.SenderStaffID, DisplayName: cb.SenderNick}
	if sender.IDValue == "" {
		// senderId is the union-style identifier the platform always provides; a
		// display name is never used as an identity.
		sender = core.Sender{IDType: "union_id", IDValue: cb.SenderID, DisplayName: cb.SenderNick}
	}
	if sender.IDValue == "" {
		return out, core.Fail("invalid_input", "callback has no sender identifier")
	}
	sentAt := epochMillis(cb.CreateAt)
	out = core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: ParseVersion,
		Origin: "stream", ProviderMessageID: cb.MsgID, ConversationID: cb.ConversationID,
		ConversationType: conversationType(cb.ConversationType), Tenant: cb.ChatbotCorpID, Sender: sender,
		Body: body, Format: "text", SentAt: sentAt, EventAt: sentAt,
		Mentioned: cb.IsInAtList, Attachments: attachments, Payload: redact(raw)}
	out.Quote = callbackQuote(cb)
	if out.Quote != nil && out.Quote.ProviderMessageID != "" {
		out.Relations = []core.Relation{{Kind: "quote", ProviderMessageID: out.Quote.ProviderMessageID, Confidence: "provider"}}
	}
	return out, nil
}

func callbackBody(cb botCallback) (string, []core.Attachment, error) {
	var body string
	var attachments []core.Attachment
	switch cb.MsgType {
	case "", "text":
		body = cb.Text.Content
	case "richText":
		// Preserve part order and explicitly identify unread media/links. Never
		// copy capability URLs or download codes into the body or attachment ID.
		var content map[string]json.RawMessage
		if len(cb.Content) == 0 || json.Unmarshal(cb.Content, &content) != nil || len(content) != 1 {
			return "", nil, core.Fail("invalid_input", "richText content is not a supported object")
		}
		partsRaw, ok := content["richText"]
		if !ok {
			return "", nil, core.Fail("invalid_input", "richText content has no parts")
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(partsRaw, &parts) != nil || len(parts) == 0 || len(parts) > maxRichTextParts {
			return "", nil, core.Fail("invalid_input", "richText parts are invalid or exceed the limit")
		}
		var joined strings.Builder
		for _, part := range parts {
			piece, attachment, err := richTextPart(part, len(attachments)+1)
			if err != nil {
				return "", nil, err
			}
			if joined.Len()+len(piece) > maxBodyBytes {
				return "", nil, core.Fail("invalid_input", "callback text exceeds the body limit")
			}
			joined.WriteString(piece)
			if attachment != nil {
				attachments = append(attachments, *attachment)
			}
		}
		body = joined.String()
	default:
		return "", nil, core.Fail("invalid_input", "callback message type is unsupported")
	}
	if len(body) > maxBodyBytes || strings.TrimSpace(body) == "" {
		return "", nil, core.Fail("invalid_input", "callback text is empty or exceeds the body limit")
	}
	return body, attachments, nil
}

func richTextPart(part map[string]json.RawMessage, ordinal int) (string, *core.Attachment, error) {
	if rawType, typed := part["type"]; typed {
		var kind string
		if json.Unmarshal(rawType, &kind) != nil || kind != "picture" {
			return "", nil, core.Fail("invalid_input", "richText contains an unsupported media part")
		}
		for key := range part {
			if key != "type" && key != "downloadCode" && key != "pictureDownloadCode" {
				return "", nil, core.Fail("invalid_input", "richText picture has unknown content")
			}
		}
		if len(part) < 2 {
			return "", nil, core.Fail("invalid_input", "richText picture has no media reference")
		}
		for _, key := range []string{"downloadCode", "pictureDownloadCode"} {
			if value, ok := part[key]; ok {
				var code string
				if json.Unmarshal(value, &code) != nil || code == "" {
					return "", nil, core.Fail("invalid_input", "richText picture has an invalid media reference")
				}
			}
		}
		name := "图片 " + strconv.Itoa(ordinal) + "（内容未读取）"
		return "[" + name + "]", &core.Attachment{Name: name, MediaType: "image/*"}, nil
	}
	if len(part) == 0 || len(part) > 2 {
		return "", nil, core.Fail("invalid_input", "richText contains an unsupported part")
	}
	textRaw, ok := part["text"]
	if !ok {
		return "", nil, core.Fail("invalid_input", "richText contains a non-text part")
	}
	var piece string
	if json.Unmarshal(textRaw, &piece) != nil {
		return "", nil, core.Fail("invalid_input", "richText has a non-string text part")
	}
	if len(part) == 1 {
		return piece, nil, nil
	}
	urlRaw, ok := part["url"]
	if !ok {
		return "", nil, core.Fail("invalid_input", "richText contains an unsupported text attribute")
	}
	var target string
	if json.Unmarshal(urlRaw, &target) != nil || target == "" {
		return "", nil, core.Fail("invalid_input", "richText has an invalid link target")
	}
	name := "链接 " + strconv.Itoa(ordinal) + "（目标未读取）"
	return piece + " [" + name + "]", &core.Attachment{Name: name, MediaType: "text/uri-list"}, nil
}

// epochMillis normalizes the platform's millisecond timestamp. An absent or
// non-positive value yields an empty string rather than the Unix epoch, so a
// missing time is visibly missing instead of silently dated 1970.
func epochMillis(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// conversationType maps the platform's numeric code to the vocabulary the store
// already uses. An unrecognized code is kept verbatim instead of being guessed
// into "group", because audience decisions depend on this value.
func conversationType(code string) string {
	switch code {
	case "1":
		return "direct"
	case "2":
		return "group"
	default:
		return code
	}
}

// Triggered reports whether the bot was actually addressed. A direct chat always
// addresses it; in a group only an explicit mention does, so ordinary group
// traffic is stored as history rather than answered.
func Triggered(e core.NormalizedEvent) bool {
	return e.ConversationType == "direct" || e.Mentioned
}
