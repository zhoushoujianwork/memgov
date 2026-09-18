package core

import (
	"context"
	"encoding/json"
	"unicode/utf8"
)

// MessageQuote is the provider-supplied snapshot, not an owner instruction or
// proof that another conversation may be queried. Unsupported types keep their
// type/identity even when no readable content was supplied.
type MessageQuote struct {
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	MessageType       string `json:"message_type,omitempty"`
	Body              string `json:"body,omitempty"`
	Title             string `json:"title,omitempty"`
	SummaryOnly       bool   `json:"summary_only,omitempty"`
	Truncated         bool   `json:"truncated,omitempty"`
}

func (q *MessageQuote) Validate() error {
	if q == nil {
		return nil
	}
	if q.ProviderMessageID != "" && !identifier.MatchString(q.ProviderMessageID) {
		return Fail("invalid_input", "quote provider_message_id is invalid")
	}
	for _, field := range []struct {
		text  string
		limit int
	}{{q.Body, 4001}, {q.Title, 257}, {q.MessageType, 129}} {
		if !utf8.ValidString(field.text) || utf8.RuneCountInString(field.text) > field.limit {
			return Fail("invalid_input", "quote text exceeds the UTF-8 boundary")
		}
	}
	return nil
}

func messageContentDigest(e NormalizedEvent) string {
	if e.Quote == nil {
		return Hash([]byte(e.Body)) // Preserve ordinary-message dedupe keys.
	}
	return Digest(struct {
		Body  string        `json:"body"`
		Quote *MessageQuote `json:"quote"`
	}{e.Body, e.Quote})
}

func quoteFromSnapshot(snapshot string) (*MessageQuote, error) {
	var value struct {
		Quote *MessageQuote `json:"quote"`
	}
	if err := json.Unmarshal([]byte(snapshot), &value); err != nil {
		return nil, err
	}
	return value.Quote, value.Quote.Validate()
}

// Read only this message's revision, never dereference a provider ID. The child
// message's availability/retention governs its received quote snapshot too.
func runtimeMessageQuote(ctx context.Context, q Queryer, id string, revision int) (*MessageQuote, error) {
	var snapshot string
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN m.availability='available' AND (`+retainedMessagePredicate("m")+`)
AND NOT EXISTS(SELECT 1 FROM source_origins so JOIN source_availability av ON av.source_id=so.source_id WHERE so.message_id=m.id AND so.revision=mr.revision AND av.state='expired') THEN mr.snapshot ELSE '{}' END
FROM messages m JOIN message_revisions mr ON mr.message_id=m.id WHERE m.id=? AND mr.revision=?`, id, revision).Scan(&snapshot)
	if err != nil {
		return nil, err
	}
	return quoteFromSnapshot(snapshot)
}

func messageRawTexts(body, snapshot string) []string {
	texts := []string{body}
	quote, _ := quoteFromSnapshot(snapshot)
	if quote != nil {
		for _, text := range []string{quote.Body, quote.Title} {
			texts = append(texts, text)
			// Native input and recovery prompts encode quotes as JSON inside a
			// string. Scrub that representation as well as the decoded raw text.
			encoded, _ := json.Marshal(text)
			escaped := string(encoded[1 : len(encoded)-1])
			if escaped != text {
				texts = append(texts, escaped)
			}
		}
	}
	return texts
}
