package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestQuoteSnapshotPersistsSeparatelyAndChangesRevision(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	e := sampleEvent("quote-child", "解释这句", Sender{IDType: "union_id", IDValue: "alice"})
	e.ProviderEventID = ""
	e.Quote = &MessageQuote{ProviderMessageID: "unseen-parent", MessageType: "text", Body: "引用原文"}
	e.Relations = []Relation{{Kind: "quote", ProviderMessageID: "unseen-parent", Confidence: "provider"}}
	first := intake(t, s, c.Name, e, "quote-first")
	if again := intake(t, s, c.Name, e, "quote-repeat"); !again.Duplicate {
		t.Fatal("quote redelivery not deduped")
	}
	messages, err := runtimeMessages(ctx, s.DB, []string{first.MessageID})
	if err != nil || len(messages) != 1 || messages[0].Body != e.Body || messages[0].Quote == nil || messages[0].Quote.Body != e.Quote.Body {
		t.Fatalf("quote roundtrip failed: %+v %v", messages, err)
	}
	var content string
	if err = s.DB.QueryRow("SELECT content FROM sources WHERE id=?", first.SourceID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "引用原文") || !strings.Contains(content, "untrusted background") {
		t.Fatal("quote missing from evidence")
	}
	e.Quote.Body = "引用更正"
	second := intake(t, s, c.Name, e, "quote-changed")
	if !second.NewRevision || second.Revision != 2 || second.SourceID == first.SourceID {
		t.Fatal("quote-only change failed to revise content")
	}
	if _, err = s.DB.Exec("UPDATE messages SET availability='recalled' WHERE id=?", first.MessageID); err != nil {
		t.Fatal(err)
	}
	messages, err = runtimeMessages(ctx, s.DB, []string{first.MessageID})
	if err != nil || messages[0].Quote != nil || messages[0].Body != "" {
		t.Fatal("recalled child quote remained readable")
	}
}

func TestExpiredQuoteRemovedFromCachedCopies(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	e := sampleEvent("expired-quote", "请求原文", Sender{IDType: "union_id", IDValue: "alice"})
	e.Quote = &MessageQuote{MessageType: "chatRecord", Body: "摘要中的敏感原文\n包含\"引号\"", Title: "敏感标题", SummaryOnly: true}
	result := intake(t, s, c.Name, e, "expired-quote-first")
	if _, err := s.DB.Exec("UPDATE messages SET availability='expired' WHERE id=?", result.MessageID); err != nil {
		t.Fatal(err)
	}
	messages, err := runtimeMessages(ctx, s.DB, []string{result.MessageID})
	if err != nil || messages[0].Quote != nil {
		t.Fatal("expired quote remained readable")
	}
	quotedJSON, _ := json.Marshal(e.Quote)
	raw, _ := json.Marshal(map[string]any{"result": "引用:" + string(quotedJSON), "quote": e.Quote})
	redacted, err := redactCachedRetentionCopy(ctx, s.DB, string(raw), nil)
	if err != nil || strings.Contains(redacted, "敏感原文") || strings.Contains(redacted, e.Quote.Title) || strings.Contains(redacted, "引号") {
		t.Fatalf("expired quote leaked from cache: %s %v", redacted, err)
	}
}

func TestQuoteRetentionReadBarrierAndPhysicalCleanup(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, route := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	var source DataSource
	runtimeMutate(t, s, "quote.retention.source", func(tx *Tx) (any, error) {
		if _, err := tx.SetChannelCapabilities(ctx, c.ID, Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); err != nil {
			return nil, err
		}
		var err error
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "quote-work", Channel: c.ID, Workspace: "global", RetentionDays: 7})
		if err != nil {
			return nil, err
		}
		source, err = tx.SyncDataSourceGroups(ctx, source.ID, []string{route.ConversationID})
		return source, err
	})
	e := sampleEvent("retention-child", "请处理", Sender{IDType: "union_id", IDValue: "alice"})
	e.Quote = &MessageQuote{MessageType: "text", Body: "待到期的引用原文"}
	e.Payload = `{"text":{"repliedMsg":{"content":"待到期的引用原文"}}}`
	result := intake(t, s, c.Name, e, "quote-retention-first")
	if _, err := s.DB.Exec("UPDATE messages SET sent_at=? WHERE id=?", time.Now().AddDate(0, 0, -8).Format(time.RFC3339Nano), result.MessageID); err != nil {
		t.Fatal(err)
	}
	messages, err := runtimeMessages(ctx, s.DB, []string{result.MessageID})
	if err != nil || messages[0].Quote != nil {
		t.Fatal("quote leaked before asynchronous cleanup")
	}
	view, err := ReadMessage(ctx, s.DB, result.MessageID)
	if err != nil || view.Quote != nil {
		t.Fatal("message API leaked expired quote")
	}
	runtimeMutate(t, s, "quote.retention.cleanup", func(tx *Tx) (any, error) {
		return tx.ApplyDataSourceRetention(ctx, source.ID, time.Now(), 20)
	})
	var snapshot, payload, content string
	if err = s.DB.QueryRow("SELECT snapshot FROM message_revisions WHERE message_id=?", result.MessageID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow("SELECT payload FROM inbox_events WHERE message_id=?", result.MessageID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow("SELECT content FROM sources WHERE id=?", result.SourceID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if snapshot != "{}" || payload != "{}" || content != "" {
		t.Fatal("quote raw data survived physical cleanup")
	}
}
