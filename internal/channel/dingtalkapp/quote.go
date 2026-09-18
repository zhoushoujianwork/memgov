package dingtalkapp

import (
	"encoding/json"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type quotedCallback struct {
	MsgID   string          `json:"msgId"`
	MsgType string          `json:"msgType"`
	Content json.RawMessage `json:"content"`
}

const maxQuoteRunes = 4000
const maxQuoteTitleRunes = 256

func callbackQuote(cb botCallback) *core.MessageQuote {
	if !cb.Text.IsReplyMsg && cb.OriginalMsgID == "" {
		return nil
	}
	out := &core.MessageQuote{ProviderMessageID: cb.OriginalMsgID}
	r := cb.Text.RepliedMsg
	if r == nil {
		return out
	}
	if r.MsgID != "" {
		out.ProviderMessageID = r.MsgID
	}
	out.MessageType, out.Truncated = boundedQuote(r.MsgType, 128)
	// Redact before decoding: a stringified payload can otherwise hide media
	// capability credentials inside its second JSON layer.
	content := r.Content
	var encoded string
	if json.Unmarshal(content, &encoded) == nil && strings.HasPrefix(strings.TrimSpace(encoded), "{") && json.Valid([]byte(encoded)) {
		var object map[string]json.RawMessage
		_ = json.Unmarshal([]byte(encoded), &object)
		// A bare text quote can itself be JSON code. Decode another layer only
		// when it has the provider's recognized content shape.
		if strings.EqualFold(r.MsgType, "text") && (object["text"] != nil || object["content"] != nil) ||
			strings.EqualFold(r.MsgType, "chatRecord") && (object["title"] != nil || object["summary"] != nil) {
			content = json.RawMessage(encoded)
		}
	}
	content = json.RawMessage(redact(content))
	switch strings.ToLower(r.MsgType) {
	case "text":
		var obj struct {
			Text    string `json:"text"`
			Content string `json:"content"`
		}
		if json.Unmarshal(content, &obj) == nil {
			out.Body = obj.Text
			if out.Body == "" {
				out.Body = obj.Content
			}
		} else {
			_ = json.Unmarshal(content, &out.Body)
		}
	case "chatrecord":
		out.SummaryOnly = true
		var obj struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
		}
		if json.Unmarshal(content, &obj) == nil {
			out.Title, out.Body = obj.Title, obj.Summary
		}
	}
	var truncated bool
	out.Body, truncated = boundedQuote(out.Body, maxQuoteRunes)
	out.Truncated = out.Truncated || truncated
	out.Title, truncated = boundedQuote(out.Title, maxQuoteTitleRunes)
	out.Truncated = out.Truncated || truncated
	return out
}

func boundedQuote(text string, limit int) (string, bool) {
	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}
	return string(runes[:limit]) + "…", true
}
