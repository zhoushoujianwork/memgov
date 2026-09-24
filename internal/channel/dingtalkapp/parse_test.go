package dingtalkapp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestBotConversationFixturesPreserveObservedGroupAndPrivateShapes(t *testing.T) {
	raw, err := os.ReadFile("testdata/bot_conversation_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string          `json:"name"`
		Frame  json.RawMessage `json:"frame"`
		Expect struct {
			ConversationType string `json:"conversation_type"`
			Mentioned        bool   `json:"mentioned"`
			Triggered        bool   `json:"triggered"`
		} `json:"expect"`
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 4 {
		t.Fatalf("conversation fixture lost required cases: %d", len(cases))
	}
	cfg := channel.Config{Tenant: "corp", Identity: core.ChannelIdentity{ExpectedCorpID: "corp", RobotCode: "fixture-bot"}}
	for _, item := range cases {
		t.Run(item.Name, func(t *testing.T) {
			event, parseErr := ParseFrame(cfg, item.Frame)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if event.ConversationType != item.Expect.ConversationType || event.Mentioned != item.Expect.Mentioned || Triggered(event) != item.Expect.Triggered {
				t.Fatalf("event=%+v expect=%+v", event, item.Expect)
			}
			if strings.Contains(event.Payload, "FIXTURE_SESSION_SECRET") || !strings.Contains(event.Payload, "[redacted]") {
				t.Fatalf("fixture credential was exposed or not exercised: %s", event.Payload)
			}
		})
	}
}

// config is the frozen receive configuration of one bound application bot. It is
// built by hand here so the parser is exercised without a database.
func config() channel.Config {
	return channel.Config{ChannelID: "ch1", ChannelName: "bot-main", Kind: core.ChannelDingTalkApp,
		Tenant: "corp1", Identity: core.ChannelIdentity{ExpectedCorpID: "corp1", ClientID: "cli-1", RobotCode: "bot-1"},
		CredentialRef: "env://MEMGOV_TEST_SECRET", Conversations: []string{"cid:group2"}}
}

// frame builds a realistic bot callback, including the session webhook the
// platform really sends, so the redaction path is under test rather than assumed.
func frame(overrides string) string {
	base := `{"conversationId":"cid:group2","conversationType":"2","msgId":"m1","msgtype":"text",` +
		`"senderId":"union-alice","senderStaffId":"staff-alice","senderNick":"Alice","senderCorpId":"corp1",` +
		`"chatbotCorpId":"corp1","chatbotUserId":"$:robot-union-1","createAt":1757000000000,"isInAtList":true,` +
		`"sessionWebhook":"https://oapi.dingtalk.com/robot/sendBySession?session=SECRET",` +
		`"sessionWebhookExpiredTime":1757000300000,"robotCode":"bot-1","text":{"content":"上线前要注意什么"}}`
	if overrides == "" {
		return base
	}
	return overrides
}

func richTextFrame(parts any) string {
	value, _ := json.Marshal(map[string]any{
		"conversationId": "cid:group2", "conversationType": "1", "msgId": "m-rich-1", "msgtype": "richText",
		"senderId": "union-alice", "senderStaffId": "staff-alice", "chatbotCorpId": "corp1",
		"chatbotUserId": "$:robot-union-1", "robotCode": "bot-1", "createAt": int64(1757000000000),
		"sessionWebhook": "https://oapi.dingtalk.com/robot/sendBySession?session=SECRET",
		"content":        map[string]any{"richText": parts},
	})
	return string(value)
}

func TestParseFrameAcceptsLiveShapedTextOnlyRichText(t *testing.T) {
	if ParseVersion != "dingtalk_app/7" {
		t.Fatalf("richText interpretation requires its own parser version: %s", ParseVersion)
	}
	for _, parts := range []any{
		[]any{map[string]any{"text": "请查一下"}},
		[]any{map[string]any{"text": "请查"}, map[string]any{"text": "一下"}},
	} {
		e, err := ParseFrame(config(), []byte(richTextFrame(parts)))
		if err != nil || e.Body != "请查一下" || e.Format != "text" || e.ConversationType != "direct" || !Triggered(e) {
			t.Fatalf("text-only richText did not become a direct-chat question: format=%s type=%s body_len=%d error=%v",
				e.Format, e.ConversationType, len(e.Body), err)
		}
		if strings.Contains(e.Payload, "SECRET") || !strings.Contains(e.Payload, "请查") {
			t.Fatal("richText evidence lost its text or kept a session credential")
		}
	}
}

func TestParseFrameAcceptsMixedRichTextWithoutExposingMediaCredentials(t *testing.T) {
	// This is the observed shape: text, a picture with two capability codes,
	// and linked text. Only the unavailable-content markers reach the Agent.
	parts := []any{
		map[string]any{"text": "请帮我看"},
		map[string]any{"text": "："},
		map[string]any{"type": "picture", "downloadCode": "MEDIA_SECRET", "pictureDownloadCode": "PICTURE_SECRET"},
		map[string]any{"text": "参考"},
		map[string]any{"text": "文档", "url": "https://example.com/ACCESS_SECRET"},
	}
	e, err := ParseFrame(config(), []byte(richTextFrame(parts)))
	if err != nil || !Triggered(e) {
		t.Fatalf("mixed richText not accepted: %v", err)
	}
	if e.Body != "请帮我看：[图片 1（内容未读取）]参考文档 [链接 2（目标未读取）]" {
		t.Fatalf("richText ordering or unreadable markers lost: %q", e.Body)
	}
	if len(e.Attachments) != 2 || e.Attachments[0].MediaType != "image/*" || e.Attachments[1].MediaType != "text/uri-list" {
		t.Fatalf("attachment types lost: %+v", e.Attachments)
	}
	for _, a := range e.Attachments {
		if a.ResourceID != "" {
			t.Fatal("stored an unverified media capability")
		}
	}
	for _, secret := range []string{"MEDIA_SECRET", "PICTURE_SECRET", "ACCESS_SECRET"} {
		if strings.Contains(e.Body, secret) || strings.Contains(e.Payload, secret) {
			t.Fatalf("mixed richText exposed a media capability: %s", secret)
		}
	}
}

func TestParseFrameRejectsIncompleteOrOversizedRichText(t *testing.T) {
	cases := map[string]any{
		"picture without reference": []any{map[string]any{"type": "picture"}},
		"picture unknown field":     []any{map[string]any{"type": "picture", "downloadCode": "MEDIA_SECRET", "caption": "hidden instruction"}},
		"picture invalid reference": []any{map[string]any{"type": "picture", "downloadCode": 123}},
		"link unknown field":        []any{map[string]any{"text": "click", "url": "https://example.com/SECRET", "content": "hidden instruction"}},
		"link invalid target":       []any{map[string]any{"text": "click", "url": 123}},
		"unknown element":           []any{map[string]any{"type": "video"}},
		"non-string text":           []any{map[string]any{"text": 123}},
		"empty array":               []any{},
		"blank text":                []any{map[string]any{"text": "  "}},
		"oversized":                 []any{map[string]any{"text": strings.Repeat("x", maxBodyBytes+1)}},
	}
	tooMany := make([]any, maxRichTextParts+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"text": "x"}
	}
	cases["too many parts"] = tooMany
	for name, parts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFrame(config(), []byte(richTextFrame(parts)))
			if core.ErrorCode(err) != "invalid_input" || strings.Contains(err.Error(), "MEDIA_SECRET") {
				t.Fatalf("unsafe richText was normalized or echoed: error=%v", err)
			}
		})
	}
	unknownContent := strings.Replace(richTextFrame([]any{map[string]any{"text": "safe"}}),
		`"richText":[{"text":"safe"}]`, `"richText":[{"text":"safe"}],"other":"ignored"`, 1)
	if _, err := ParseFrame(config(), []byte(unknownContent)); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("extra content was silently ignored: %v", err)
	}
	plainLong := strings.Replace(frame(""), "上线前要注意什么", strings.Repeat("x", maxBodyBytes+1), 1)
	if _, err := ParseFrame(config(), []byte(plainLong)); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("plain text bypassed the body limit: %v", err)
	}
}

func TestRedactStripsNestedRichMediaCapabilityCodes(t *testing.T) {
	raw := richTextFrame([]any{map[string]any{"type": "picture", "downloadCode": "MEDIA_SECRET", "pictureDownloadCode": "PICTURE_SECRET"}})
	redacted := redact([]byte(raw))
	for _, secret := range []string{"MEDIA_SECRET", "PICTURE_SECRET", "session=SECRET"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("a nested capability was retained in rejected evidence: %s", secret)
		}
	}
}

// A normal callback becomes one message event, and the credentials it carried
// never reach the stored payload.
func TestParseFrameNormalizesAndStripsCredentials(t *testing.T) {
	e, err := ParseFrame(config(), []byte(frame("")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Kind != core.EventMessage || e.Adapter != "dingtalk_app" || e.ParseVersion != ParseVersion {
		t.Fatalf("event identity: %+v", e)
	}
	if e.ProviderMessageID != "m1" || e.ConversationID != "cid:group2" || e.ConversationType != "group" {
		t.Fatalf("event target: %+v", e)
	}
	if e.Sender.IDType != "user_id" || e.Sender.IDValue != "staff-alice" {
		t.Fatalf("sender: %+v", e.Sender)
	}
	if e.Body != "上线前要注意什么" || !e.Mentioned {
		t.Fatalf("body or mention: %+v", e)
	}
	if e.SentAt == "" || e.SentAt != e.EventAt {
		t.Fatalf("timestamps: %q %q", e.SentAt, e.EventAt)
	}
	// The bearer credentials must not be in the payload that goes to SQLite, a
	// log, a model context or an export.
	for _, secret := range []string{"SECRET", "sendBySession", "1757000300000"} {
		if strings.Contains(e.Payload, secret) {
			t.Fatalf("payload still carries %q: %s", secret, e.Payload)
		}
	}
	if !strings.Contains(e.Payload, "[redacted]") || !strings.Contains(e.Payload, "上线前要注意什么") {
		t.Fatalf("payload lost its evidence value: %s", e.Payload)
	}
	if !Triggered(e) {
		t.Fatal("an @ mention in a group did not trigger")
	}
}

// A group message that did not address the bot is stored but does not trigger a
// reply, and a direct message always does.
func TestTriggeredRequiresBeingAddressed(t *testing.T) {
	quiet := `{"conversationId":"cid:group2","conversationType":"2","msgId":"m2","msgtype":"text",` +
		`"senderStaffId":"staff-bob","chatbotCorpId":"corp1","chatbotUserId":"$:robot-union-1","robotCode":"bot-1","createAt":1757000001000,` +
		`"isInAtList":false,"text":{"content":"随口聊两句"}}`
	e, err := ParseFrame(config(), []byte(quiet))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if Triggered(e) {
		t.Fatal("ordinary group traffic triggered the bot")
	}
	direct := strings.Replace(quiet, `"conversationType":"2"`, `"conversationType":"1"`, 1)
	e, err = ParseFrame(config(), []byte(direct))
	if err != nil {
		t.Fatalf("parse direct: %v", err)
	}
	if e.ConversationType != "direct" || !Triggered(e) {
		t.Fatalf("a direct message did not trigger: %+v", e)
	}
}

// A callback that belongs to another tenant or another robot is refused. A
// misdirected subscription must not write into this channel's history.
func TestParseFrameRefusesForeignTenantAndRobot(t *testing.T) {
	foreign := strings.Replace(frame(""), `"chatbotCorpId":"corp1"`, `"chatbotCorpId":"corp9"`, 1)
	if _, err := ParseFrame(config(), []byte(foreign)); core.ErrorCode(err) != "denied" {
		t.Fatalf("a foreign tenant was accepted: %v", err)
	}
	missingCorp := strings.Replace(frame(""), `"chatbotCorpId":"corp1",`, "", 1)
	if _, err := ParseFrame(config(), []byte(missingCorp)); core.ErrorCode(err) != "denied" {
		t.Fatalf("a callback without its enterprise was accepted: %v", err)
	}
	other := strings.Replace(frame(""), `"robotCode":"bot-1"`, `"robotCode":"bot-9"`, 1)
	if _, err := ParseFrame(config(), []byte(other)); core.ErrorCode(err) != "denied" {
		t.Fatalf("a callback for another robot was accepted: %v", err)
	}
	missingCode := strings.Replace(frame(""), `"robotCode":"bot-1",`, "", 1)
	if _, err := ParseFrame(config(), []byte(missingCode)); err != nil {
		t.Fatalf("a callback without the SDK-optional robotCode was rejected: %v", err)
	}
	// A real chatbotUserId is a DingTalk user identifier, separate from the
	// robotCode used by outbound APIs. Its different bytes must not deny a valid
	// callback from the application bot authenticated by this Stream session.
	if _, err := ParseFrame(config(), []byte(frame(""))); err != nil {
		t.Fatalf("the callback user identifier was mistaken for a robot code: %v", err)
	}
}

// Unsupported and malformed frames are refused with invalid_input so the caller
// isolates them, instead of being coerced into an empty message.
func TestParseFrameRefusesUnsupportedAndMalformedFrames(t *testing.T) {
	cases := map[string]string{
		"image message":   strings.Replace(frame(""), `"msgtype":"text"`, `"msgtype":"picture"`, 1),
		"missing msgId":   `{"conversationId":"cid:group2","msgtype":"text","senderStaffId":"s","chatbotCorpId":"corp1","chatbotUserId":"$:robot-union-1","robotCode":"bot-1","text":{"content":"x"}}`,
		"no sender":       `{"conversationId":"cid:group2","msgId":"m3","msgtype":"text","senderNick":"Alice","chatbotCorpId":"corp1","chatbotUserId":"$:robot-union-1","robotCode":"bot-1","text":{"content":"x"}}`,
		"not json":        `{"conversationId":`,
		"oversized frame": `{"text":{"content":"` + strings.Repeat("x", maxFrameBytes) + `"}}`,
	}
	for name, raw := range cases {
		if _, err := ParseFrame(config(), []byte(raw)); core.ErrorCode(err) != "invalid_input" {
			t.Fatalf("%s was accepted: %v", name, err)
		}
	}
}

// A payload that cannot be parsed but mentions a webhook is withheld entirely.
// Keeping it verbatim would risk storing a live credential.
func TestRedactWithholdsUnparseablePayloadThatMayHoldACredential(t *testing.T) {
	out := redact([]byte(`{"sessionWebhook":"https://oapi.dingtalk.com/x?session=SECRET"`))
	if strings.Contains(out, "SECRET") || !strings.Contains(out, "withheld") {
		t.Fatalf("an unparseable payload leaked or was not withheld: %s", out)
	}
	// A truncated payload with no credential marker stays as evidence.
	out = redact([]byte(`{"msgId":"m1"`))
	if !strings.Contains(out, "m1") {
		t.Fatalf("a harmless truncated payload was dropped: %s", out)
	}
}

// A missing timestamp stays missing rather than becoming the Unix epoch.
func TestEpochMillisLeavesAnAbsentTimeEmpty(t *testing.T) {
	if got := epochMillis(0); got != "" {
		t.Fatalf("an absent time became %q", got)
	}
	if got := epochMillis(1757000000000); got != "2025-09-04T15:33:20Z" {
		t.Fatalf("unexpected normalization: %q", got)
	}
}

func TestParseFrameSenderUsesExactUserIDAndUnionFallback(t *testing.T) {
	if ParseVersion != "dingtalk_app/7" {
		t.Fatalf("identity interpretation must have its own parser version: %s", ParseVersion)
	}
	for _, tc := range []struct {
		name, staff, union, wantType, wantValue string
	}{
		{"user ID wins", "opaque-user", "different-union", "user_id", "opaque-user"},
		{"same bytes do not change type", "same", "same", "user_id", "same"},
		{"fallback", "", "opaque-union", "union_id", "opaque-union"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.ReplaceAll(frame(""), "staff-alice", tc.staff)
			raw = strings.ReplaceAll(raw, "union-alice", tc.union)
			e, err := ParseFrame(config(), []byte(raw))
			if err != nil || e.Sender.IDType != tc.wantType || e.Sender.IDValue != tc.wantValue {
				t.Fatalf("exact sender: %+v, %v", e.Sender, err)
			}
		})
	}
}
