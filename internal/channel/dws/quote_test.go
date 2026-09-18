package dws

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
)

func TestReadWindowParsesProjectedQuotedMessageText(t *testing.T) {
	start := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	page := `{"messages":[{"messageId":"reply-1","text":"引用后的请求","time":"2026-09-18T09:01:00Z","senderId":"owner","quotedMessage":{"messageId":"original-1","text":"这是被引用的原始内容","sender":"OpenClaw小钉-周守健"}}],"complete":true,"hasMore":false}`
	a := &Adapter{Timeout: time.Second, Run: func(_ context.Context, args ...string) ([]byte, error) {
		return envelope(page), nil
	}}
	out, err := a.ReadWindow(context.Background(), testConfig(), channel.Window{
		ConversationID: "cid:group1", ConversationType: "group", Start: start, End: start.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 {
		t.Fatalf("events: %+v complete=%v stop=%s err=%v", out.Events, out.Complete, out.StopReason, err)
	}
	e := out.Events[0]
	if e.Quote == nil || e.Quote.ProviderMessageID != "original-1" || e.Quote.Body != "这是被引用的原始内容" {
		t.Fatalf("quote was not normalized: %+v", e.Quote)
	}
	if len(e.Relations) != 1 || e.Relations[0].ProviderMessageID != "original-1" {
		t.Fatalf("quote relation was not preserved: %+v", e.Relations)
	}
}

func TestParseEventParsesQuotedMessageAndKeepsOwnerRequestSeparate(t *testing.T) {
	line := `{"event_type":"user_im_message_receive_group","event_corp_id":"corp1","data":"{\"msgId\":\"reply-2\",\"conversationId\":\"cid:group1\",\"senderId\":\"owner\",\"senderNick\":\"Owner\",\"text\":{\"content\":\"请处理引用\"},\"originalMsgId\":\"original-2\",\"quotedMessage\":{\"messageId\":\"original-2\",\"msgtype\":\"text\",\"text\":\"被引用的内容\"}}"}`
	e, err := ParseEvent(testConfig(), []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if e.Body != "请处理引用" || e.Quote == nil || e.Quote.Body != "被引用的内容" || e.Quote.ProviderMessageID != "original-2" {
		t.Fatalf("event quote: body=%q quote=%+v", e.Body, e.Quote)
	}
	if strings.Contains(e.Body, e.Quote.Body) {
		t.Fatal("the quote must remain separate from the verified message body")
	}
}

func TestDWSQuoteBoundsContent(t *testing.T) {
	quote := dwsMessageQuote("original-long", &dwsQuotedMessage{MsgType: "text", Text: strings.Repeat("中", maxDWSQuoteRunes+10)})
	if quote == nil || !quote.Truncated || len([]rune(quote.Body)) != maxDWSQuoteRunes+1 {
		t.Fatalf("long quote was not bounded: %+v", quote)
	}
}

func TestDWSQuoteWithoutProjectedBodyRemainsUnreadable(t *testing.T) {
	quote := dwsMessageQuote("original-3", nil)
	if quote == nil || quote.ProviderMessageID != "original-3" || quote.Body != "" {
		t.Fatalf("id-only quote should not fabricate content: %+v", quote)
	}
}

func TestDWSQuoteRedactsEmbeddedCapabilityFields(t *testing.T) {
	quote := dwsMessageQuote("original-4", &dwsQuotedMessage{
		MsgType: "text",
		Text:    `{"text":"safe","sessionWebhook":"https://secret.example"}`,
	})
	if quote == nil || strings.Contains(quote.Body, "https://secret.example") || !strings.Contains(quote.Body, "redacted") {
		t.Fatalf("quoted capability was not redacted: %+v", quote)
	}
}
