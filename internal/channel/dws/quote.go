package dws

import (
	"encoding/json"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

const (
	maxDWSQuoteRunes      = 4000
	maxDWSQuoteTitleRunes = 256
)

// dwsQuotedMessage is the provider's projected quotedMessage shape. DWS may
// expose either text/content for a text message, or title/summary for a
// chat-record summary. We never dereference a provider ID to manufacture text.
type dwsQuotedMessage struct {
	ID      string `json:"messageId"`
	MsgType string `json:"msgtype"`
	Text    string `json:"text"`
	Content string `json:"content"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

func dwsMessageQuote(providerID string, quoted *dwsQuotedMessage) *core.MessageQuote {
	if providerID == "" && quoted == nil {
		return nil
	}
	out := &core.MessageQuote{ProviderMessageID: providerID}
	if quoted == nil {
		return out
	}
	if quoted.ID != "" {
		out.ProviderMessageID = quoted.ID
	}
	out.MessageType, out.Truncated = boundedDWSQuote(quoted.MsgType, 128)
	if strings.EqualFold(out.MessageType, "chatrecord") {
		out.SummaryOnly = true
		var truncated bool
		out.Title, truncated = boundedDWSQuote(redactDWSQuote(quoted.Title), maxDWSQuoteTitleRunes)
		out.Truncated = out.Truncated || truncated
		out.Body, truncated = boundedDWSQuote(redactDWSQuote(firstNonEmpty(quoted.Summary, quoted.Text, quoted.Content)), maxDWSQuoteRunes)
		out.Truncated = out.Truncated || truncated
	} else {
		var truncated bool
		out.Body, truncated = boundedDWSQuote(redactDWSQuote(firstNonEmpty(quoted.Text, quoted.Content)), maxDWSQuoteRunes)
		out.Truncated = out.Truncated || truncated
	}
	if quoted.MsgType == "" && out.Body != "" {
		out.MessageType = "text"
	}
	return out
}

func boundedDWSQuote(value string, limit int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= limit {
		return value, false
	}
	return string(runes[:limit]) + "…", true
}

// DWS normally returns quoted text as a string. If a provider version returns
// a JSON-encoded object instead, redact capability-bearing fields before the
// text can enter evidence or model context.
func redactDWSQuote(value string) string {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var decoded any
		if json.Unmarshal([]byte(trimmed), &decoded) == nil {
			return core.JSON(redactDWSFields(decoded))
		}
	}
	if containsDWSSecret(trimmed) {
		return "[redacted quoted content]"
	}
	return value
}

func containsDWSSecret(value string) bool {
	for _, key := range []string{"sessionWebhook", "sessionWebhookExpiredTime", "downloadCode", "pictureDownloadCode", "mediaId", "fileId"} {
		if strings.Contains(value, key) {
			return true
		}
	}
	return false
}

func redactDWSFields(value any) any {
	secret := map[string]bool{
		"sessionWebhook": true, "sessionWebhookExpiredTime": true, "downloadCode": true,
		"pictureDownloadCode": true, "mediaId": true, "fileId": true, "url": true,
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if secret[key] {
				typed[key] = "[redacted]"
			} else {
				typed[key] = redactDWSFields(item)
			}
		}
	case []any:
		for i, item := range typed {
			typed[i] = redactDWSFields(item)
		}
	}
	return value
}
