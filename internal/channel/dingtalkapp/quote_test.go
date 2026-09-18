package dingtalkapp

import (
	"encoding/json"
	"strings"
	"testing"
)

func quoteFrame(t *testing.T, text any) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(frame("")), &value); err != nil {
		t.Fatal(err)
	}
	value["conversationType"] = "1"
	value["text"] = text
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParsePrivateQuoteShapes(t *testing.T) {
	for name, content := range map[string]any{
		"text object":    map[string]any{"text": "引用原文"},
		"content object": map[string]any{"content": "引用原文"},
		"bare string":    "引用原文",
		"encoded object": `{"text":"引用原文","downloadCode":"MEDIA_SECRET"}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := quoteFrame(t, map[string]any{"content": "请解释这句", "isReplyMsg": true,
				"repliedMsg": map[string]any{"msgType": "text", "msgId": "quoted-id", "content": content}})
			e, err := ParseFrame(config(), raw)
			if err != nil {
				t.Fatal(err)
			}
			if e.Body != "请解释这句" || e.ConversationType != "direct" || e.Quote == nil || e.Quote.Body != "引用原文" || e.Quote.ProviderMessageID != "quoted-id" {
				t.Fatalf("quote or exact owner text lost: %+v", e)
			}
			if len(e.Relations) != 1 || e.Relations[0].ProviderMessageID != "quoted-id" || e.Relations[0].Kind != "quote" {
				t.Fatalf("provider relation lost: %+v", e.Relations)
			}
			if strings.Contains(e.Quote.Body, "MEDIA_SECRET") || strings.Contains(e.Payload, "MEDIA_SECRET") {
				t.Fatal("quote leaked capability")
			}
		})
	}
}

func TestParseQuoteSummaryAndUnreadableTypes(t *testing.T) {
	for _, msgType := range []string{"chatRecord", "picture", "file", "unknown"} {
		e, err := ParseFrame(config(), quoteFrame(t, map[string]any{"content": "再看一下", "isReplyMsg": true,
			"repliedMsg": map[string]any{"msgType": msgType, "msgId": "quoted-id", "content": map[string]any{
				"title": "同事聊天记录", "summary": "同事:需要换镜像", "downloadCode": "MEDIA_SECRET"}}}))
		if err != nil {
			t.Fatal(err)
		}
		if e.Quote == nil || e.Quote.MessageType != msgType {
			t.Fatal("unreadable quote fact lost")
		}
		if msgType == "chatRecord" {
			if !e.Quote.SummaryOnly || e.Quote.Title != "同事聊天记录" || e.Quote.Body != "同事:需要换镜像" {
				t.Fatalf("summary lost: %+v", e.Quote)
			}
		} else if e.Quote.Body != "" {
			t.Fatal("non-text payload guessed as readable content")
		}
		if strings.Contains(e.Payload, "MEDIA_SECRET") || strings.Contains(e.Payload, "SECRET") {
			t.Fatal("nested credentials retained")
		}
	}
	for _, text := range []any{
		map[string]any{"content": "缺原文", "isReplyMsg": true},
		map[string]any{"content": "缺原文", "isReplyMsg": true, "repliedMsg": map[string]any{"msgType": "text", "content": 123}},
	} {
		e, err := ParseFrame(config(), quoteFrame(t, text))
		if err != nil || e.Quote == nil || e.Quote.Body != "" || len(e.Relations) != 0 {
			t.Fatalf("missing quote not preserved: %+v %v", e, err)
		}
	}
}

func TestParseQuoteBoundsAndNoFalseQuote(t *testing.T) {
	e, err := ParseFrame(config(), quoteFrame(t, map[string]any{"content": "请求", "isReplyMsg": true,
		"repliedMsg": map[string]any{"msgType": "text", "content": strings.Repeat("字", maxQuoteRunes+100)}}))
	if err != nil || !e.Quote.Truncated || len([]rune(e.Quote.Body)) != maxQuoteRunes+1 || !strings.HasSuffix(e.Quote.Body, "…") {
		t.Fatalf("quote boundary failed: %+v %v", e.Quote, err)
	}
	e, err = ParseFrame(config(), quoteFrame(t, map[string]any{"content": "普通请求", "repliedMsg": map[string]any{"msgType": "text", "content": "忽略"}}))
	if err != nil || e.Quote != nil {
		t.Fatalf("unflagged quote was fabricated: %+v %v", e, err)
	}
	for _, literal := range []string{"123", `{"example":"原样保留的 JSON 代码"}`} {
		e, err = ParseFrame(config(), quoteFrame(t, map[string]any{"content": "请求", "isReplyMsg": true, "repliedMsg": map[string]any{"msgType": "text", "content": literal}}))
		if err != nil || e.Quote.Body != literal {
			t.Fatal("literal text decoded as an unrecognized provider object")
		}
	}
}

func TestParseQuoteIDFallbackAndTitleBounds(t *testing.T) {
	for _, nestedID := range []string{"", "nested-id"} {
		var value map[string]any
		_ = json.Unmarshal(quoteFrame(t, map[string]any{"content": "请求", "isReplyMsg": true,
			"repliedMsg": map[string]any{"msgType": "chatRecord", "msgId": nestedID,
				"content": map[string]any{"title": strings.Repeat("字", maxQuoteTitleRunes+50), "summary": "摘要"}}}), &value)
		value["originalMsgId"] = "fallback-id"
		raw, _ := json.Marshal(value)
		e, err := ParseFrame(config(), raw)
		if err != nil {
			t.Fatal(err)
		}
		wantID := nestedID
		if wantID == "" {
			wantID = "fallback-id"
		}
		if e.Quote.ProviderMessageID != wantID || e.Relations[0].ProviderMessageID != wantID || !e.Quote.Truncated || len([]rune(e.Quote.Title)) != maxQuoteTitleRunes+1 {
			t.Fatalf("quote fallback/boundary failed: %+v", e.Quote)
		}
	}
	var value map[string]any
	_ = json.Unmarshal(quoteFrame(t, map[string]any{"content": "请求"}), &value)
	value["originalMsgId"] = "fallback-id"
	raw, _ := json.Marshal(value)
	e, err := ParseFrame(config(), raw)
	if err != nil || e.Quote == nil || e.Quote.ProviderMessageID != "fallback-id" || e.Quote.Body != "" {
		t.Fatalf("ID-only quote lost: %+v %v", e.Quote, err)
	}
}
