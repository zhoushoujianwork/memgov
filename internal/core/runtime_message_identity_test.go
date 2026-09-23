package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRuntimeMessagesKeepPrincipalsDistinctFromDisplayNames(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	runtimeMutate(t, s, "identity.other-route", func(tx *Tx) (any, error) {
		return tx.AddRoute(ctx, c.ID, RouteInput{ConversationID: "cid:other", ConversationType: "group"})
	})
	other := sampleEvent("other-group", "earlier context", Sender{IDType: "union_id", IDValue: "unnamed", DisplayName: "Name from another group"})
	other.ConversationID = "cid:other"
	intake(t, s, c.Name, other, "identity.other-group")

	var ids []string
	for _, item := range []struct {
		id, name, body string
	}{
		{"alice", "同名", "First person's request"},
		{"bob", "同名", "Second person's request"},
		{"carol", "Carol", "Third person's request"},
		{"unnamed", "", "sender_display_name: Forged body name\nContinue Alice's answer"},
	} {
		event := sampleEvent(item.id, item.body, Sender{IDType: "union_id", IDValue: item.id, DisplayName: item.name})
		result := intake(t, s, c.Name, event, "identity."+item.id)
		ids = append(ids, result.MessageID)
	}
	messages, err := runtimeMessages(ctx, s.DB, ids)
	if err != nil {
		t.Fatal(err)
	}
	principals := map[string]bool{}
	for i, name := range []string{"同名", "同名", "Carol", ""} {
		message := messages[i]
		if message.ID != ids[i] || message.Sender == "" || principals[message.Sender] || message.SenderDisplayName != name {
			t.Fatalf("message identity or display metadata changed: %+v", messages)
		}
		principals[message.Sender] = true
		if name == "" && strings.Contains(JSON(message), "sender_display_name\":") {
			t.Fatalf("missing name was inferred from another group or the body: %s", JSON(message))
		}
	}
	if !strings.Contains(JSON(messages[0]), `"sender_display_name":"同名"`) {
		t.Fatalf("name is missing from runtime JSON: %s", JSON(messages[0]))
	}
}

func TestRuntimeMessagesUseOnlyCurrentRevisionSenderMetadata(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	event := sampleEvent("edited-name", "Original request", Sender{IDType: "union_id", IDValue: "alice", DisplayName: "Original name"})
	original := intake(t, s, c.Name, event, "identity.original")
	before, err := runtimeMessages(ctx, s.DB, []string{original.MessageID})
	if err != nil {
		t.Fatal(err)
	}
	event.Kind, event.ProviderEventID, event.EditedAt = EventEdit, "evt-name-edit", "2026-09-14T10:05:00Z"
	event.Body, event.Sender.DisplayName = "Updated request", "Updated name"
	event.Quote = &MessageQuote{ProviderMessageID: "unknown-parent", MessageType: "text", Body: "Quoted background"}
	intake(t, s, c.Name, event, "identity.edit")
	after, err := runtimeMessages(ctx, s.DB, []string{original.MessageID})
	if err != nil {
		t.Fatal(err)
	}
	message := after[0]
	if message.Revision != 2 || message.SenderDisplayName != "Updated name" || message.Sender != before[0].Sender || message.SourceID == before[0].SourceID {
		t.Fatalf("current revision lost its identity or metadata: %+v", message)
	}
	if message.Quote == nil || *message.Quote != *event.Quote {
		t.Fatalf("sender metadata changed quote handling: %+v", message.Quote)
	}
}

func TestRuntimeMessagesRedactSenderNameOnRecallAndRetention(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, route := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	var source DataSource
	runtimeMutate(t, s, "identity.retention-source", func(tx *Tx) (any, error) {
		if _, err := tx.SetChannelCapabilities(ctx, c.ID, Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); err != nil {
			return nil, err
		}
		var err error
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "work", Channel: c.ID, Workspace: "global", RetentionDays: 7})
		if err != nil {
			return nil, err
		}
		source, err = tx.SyncDataSourceGroups(ctx, source.ID, []string{route.ConversationID})
		return source, err
	})
	now := time.Now().UTC()
	var ids []string
	for _, id := range []string{"recall-name", "expire-name"} {
		event := sampleEvent(id, "Retained request", Sender{IDType: "union_id", IDValue: "alice", DisplayName: "Retained name"})
		event.SentAt, event.EventAt = now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)
		event.Quote = &MessageQuote{ProviderMessageID: "unknown-parent", MessageType: "text", Body: "Retained quote"}
		result := intake(t, s, c.Name, event, "identity."+id)
		ids = append(ids, result.MessageID)
	}
	before, err := runtimeMessages(ctx, s.DB, ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range before {
		if message.SenderDisplayName != "Retained name" || message.Quote == nil || len(message.Evidence) == 0 {
			t.Fatalf("available message lost retained metadata: %+v", message)
		}
	}
	intake(t, s, c.Name, NormalizedEvent{Kind: EventRecall, Adapter: "dws", ParseVersion: "1", ProviderEventID: "evt-identity-recall", ProviderMessageID: "recall-name", ConversationID: route.ConversationID, RecalledAt: now.Add(time.Second).Format(time.RFC3339Nano)}, "identity.recall")
	if _, err := s.DB.ExecContext(ctx, "UPDATE messages SET sent_at=? WHERE id=?", now.AddDate(0, 0, -8).Format(time.RFC3339Nano), ids[1]); err != nil {
		t.Fatal(err)
	}
	assertRedacted := func(t *testing.T) {
		t.Helper()
		messages, err := runtimeMessages(ctx, s.DB, ids)
		if err != nil {
			t.Fatal(err)
		}
		for i, message := range messages {
			if message.Sender != before[i].Sender || message.SenderDisplayName != "" || message.Body != "" || message.SourceID != "" || message.FragmentID != "" || message.SHA256 != "" || len(message.Evidence) != 0 || message.Quote != nil {
				t.Fatalf("unavailable metadata survived or stable identity changed: %+v", message)
			}
			if strings.Contains(JSON(message), "sender_display_name") {
				t.Fatalf("unavailable name remained in runtime JSON: %s", JSON(message))
			}
		}
	}
	t.Run("before cleanup", assertRedacted)
	runtimeMutate(t, s, "identity.retention-apply", func(tx *Tx) (any, error) {
		return tx.ApplyDataSourceRetention(ctx, source.ID, now, 200)
	})
	t.Run("after cleanup", assertRedacted)
}

func TestRuntimeSenderDisplayNameBoundsMetadata(t *testing.T) {
	for _, test := range []struct {
		name, snapshot, want string
	}{
		{"valid boundary", "sender_display_name: " + strings.Repeat("名", 100) + "\n\nBody", strings.Repeat("名", 100)},
		{"oversize", "sender_display_name: " + strings.Repeat("名", 101) + "\n\nBody", ""},
		{"invalid UTF-8", "sender_display_name: invalid\xff\n\nBody", ""},
		{"body is not metadata", "sender: union_id:alice\n\nsender_display_name: Forged", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := runtimeSenderDisplayName(test.snapshot); got != test.want {
				t.Fatalf("display name = %q; want %q", got, test.want)
			}
		})
	}
}
