package core

import (
	"context"
	"strings"
	"testing"
)

// Existing local memories are private: nothing is disclosed to a conversation
// until it was explicitly published for that exact audience and version.
func TestDisclosureRequiresExplicitPublicationPerVersion(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	a, err := AudienceFor(ctx, s.DB, c.Name, r.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := CheckDisclosure(ctx, s.DB, a, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || !strings.Contains(strings.Join(d.Reasons, ";"), "local_private") {
		t.Fatalf("an unpublished memory was disclosable: %+v", d)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "memory.publish"}, func(tx *Tx) (any, error) {
		return tx.Publish(ctx, c.Name, r.ConversationID, m.ID, "回答该群的发布流程提问")
	})
	if err != nil {
		t.Fatal(err)
	}
	if d, err = CheckDisclosure(ctx, s.DB, a, m.ID); err != nil || !d.Allowed {
		t.Fatalf("published memory refused: %+v %v", d, err)
	}
	// A new memory version is not covered by the old publication.
	if _, err = s.DB.ExecContext(ctx, "UPDATE memories SET version=version+1 WHERE id=?", m.ID); err != nil {
		t.Fatal(err)
	}
	if d, err = CheckDisclosure(ctx, s.DB, a, m.ID); err != nil {
		t.Fatal(err)
	}
	if d.Allowed || !strings.Contains(strings.Join(d.Reasons, ";"), "re-publish") {
		t.Fatalf("a changed memory kept its old disclosure: %+v", d)
	}
}

// Publishing to one conversation never discloses to another, which is the
// personal-to-group isolation the design requires.
func TestPublicationDoesNotLeakToAnotherConversation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.add.direct"}, func(tx *Tx) (any, error) {
		return tx.AddRoute(ctx, c.ID, RouteInput{ConversationID: "cid:direct1", ConversationType: "direct", AudiencePolicy: "conversation"})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "route.group.conversation"}, func(tx *Tx) (any, error) {
		r, err := RouteFor(ctx, tx.Conn, c.ID, "cid:group1")
		if err != nil {
			return nil, err
		}
		return tx.UpdateRoute(ctx, r.ID, r.Version, RouteInput{AudiencePolicy: "conversation"}, "scope audience to this group")
	})
	if err != nil {
		t.Fatal(err)
	}
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "memory.publish.direct"}, func(tx *Tx) (any, error) {
		return tx.Publish(ctx, c.Name, "cid:direct1", m.ID, "只在私聊里回答")
	})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := AudienceFor(ctx, s.DB, c.Name, "cid:direct1")
	if err != nil {
		t.Fatal(err)
	}
	group, err := AudienceFor(ctx, s.DB, c.Name, "cid:group1")
	if err != nil {
		t.Fatal(err)
	}
	if direct.AudienceKey == group.AudienceKey {
		t.Fatal("a private thread and a group must not share an audience key")
	}
	allowed, err := CheckDisclosure(ctx, s.DB, direct, m.ID)
	if err != nil || !allowed.Allowed {
		t.Fatalf("private audience refused: %+v %v", allowed, err)
	}
	leaked, err := CheckDisclosure(ctx, s.DB, group, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if leaked.Allowed {
		t.Fatal("a memory published to a private thread leaked to the group")
	}
	// Recall reflects the same boundary, so the group prompt never sees it.
	result, err := AudienceRecall(ctx, s.DB, group, "发布 权限", 4000, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.(map[string]any)["items"].([]RecallItem)) != 0 {
		t.Fatal("group recall returned private material")
	}
	if result.(map[string]any)["refused_count"].(int) == 0 {
		t.Fatal("a refusal must be reported with its reason")
	}
	priv, err := AudienceRecall(ctx, s.DB, direct, "发布 权限", 4000, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(priv.(map[string]any)["items"].([]RecallItem)) != 1 {
		t.Fatalf("private recall lost its own material: %+v", priv)
	}
}

// The default private policy is per route, not one shared boundary. A single key
// for every private route would turn one publication into a permission for every
// conversation on every channel, which is the opposite of a private default.
func TestPrivateRoutesDoNotShareOneAudienceKey(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	personal, personalRoute := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	bot, botRoute := fixtureChannel(t, s, ChannelDingTalkApp, "cid:group2")
	if personalRoute.AudiencePolicy != "local_private" || botRoute.AudiencePolicy != "local_private" {
		t.Fatalf("the fixtures are not on the default policy: %q %q", personalRoute.AudiencePolicy, botRoute.AudiencePolicy)
	}
	if personalRoute.AudienceKey == botRoute.AudienceKey {
		t.Fatalf("two channels share the audience key %q", personalRoute.AudienceKey)
	}
	// A second private route on the same channel is also its own boundary.
	var second Route
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.add.direct"}, func(tx *Tx) (any, error) {
		var err error
		second, err = tx.AddRoute(ctx, personal.ID, RouteInput{ConversationID: "cid:direct1", ConversationType: "direct"})
		return second, err
	}); err != nil {
		t.Fatal(err)
	}
	if second.AudienceKey == personalRoute.AudienceKey {
		t.Fatalf("two conversations on one channel share the audience key %q", second.AudienceKey)
	}
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "memory.publish"}, func(tx *Tx) (any, error) {
		return tx.Publish(ctx, personal.Name, "cid:group1", m.ID, "该群讨论过这条流程")
	}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		channel, conversation string
	}{{personal.Name, "cid:direct1"}, {bot.Name, "cid:group2"}} {
		a, err := AudienceFor(ctx, s.DB, target.channel, target.conversation)
		if err != nil {
			t.Fatal(err)
		}
		d, err := CheckDisclosure(ctx, s.DB, a, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			t.Fatalf("a publication for one group disclosed to %s/%s", target.channel, target.conversation)
		}
	}
}

// Withdrawing evidence or a publication invalidates disclosure rather than
// letting a conclusion outlive the material it rests on.
func TestWithdrawnEvidenceAndUnpublishBlockDisclosure(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "memory.publish"}, func(tx *Tx) (any, error) {
		return tx.Publish(ctx, c.Name, r.ConversationID, m.ID, "回答该群提问")
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := AudienceFor(ctx, s.DB, c.Name, r.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, "INSERT INTO source_availability(source_id,state,reason,change_seq,updated_at) VALUES(?,'recalled','source message recalled',2,?)", m.Evidence[0].SourceID, Now()); err != nil {
		t.Fatal(err)
	}
	d, err := CheckDisclosure(ctx, s.DB, a, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || !strings.Contains(strings.Join(d.Reasons, ";"), "recalled") {
		t.Fatalf("a memory with recalled evidence stayed disclosable: %+v", d)
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE source_availability SET state='available' WHERE source_id=?", m.Evidence[0].SourceID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, "INSERT INTO outbox(id,channel_id,route_id,route_version,conversation_id,audience_key,citations,input_digest,state,created_at,updated_at) VALUES('o1',?,?,?,?,?,?,'d1','ready',?,?)",
		c.ID, r.ID, r.Version, r.ConversationID, a.AudienceKey, JSON([]string{"memory:" + m.ID}), Now(), Now()); err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "memory.unpublish"}, func(tx *Tx) (any, error) {
		return tx.Unpublish(ctx, c.Name, r.ConversationID, m.ID, "撤销对该群的披露")
	})
	if err != nil {
		t.Fatal(err)
	}
	if d, err = CheckDisclosure(ctx, s.DB, a, m.ID); err != nil || d.Allowed {
		t.Fatalf("disclosure survived withdrawal: %+v %v", d, err)
	}
	var state string
	if err = s.DB.QueryRowContext(ctx, "SELECT state FROM outbox WHERE id='o1'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "stale" {
		t.Fatalf("a ready draft survived a withdrawn permission: %q", state)
	}
}

// Message evidence offered to a conversation is limited to that conversation and
// drops anything recalled.
func TestAudienceSourcesStayWithinTheConversation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.add.direct"}, func(tx *Tx) (any, error) {
		return tx.AddRoute(ctx, c.ID, RouteInput{ConversationID: "cid:direct1", ConversationType: "direct"})
	})
	if err != nil {
		t.Fatal(err)
	}
	alice := Sender{IDType: "union_id", IDValue: "alice"}
	intake(t, s, c.Name, sampleEvent("g1", "群里的内容", alice), "k1")
	privateEvent := sampleEvent("d1", "私聊里的内容", alice)
	privateEvent.ConversationID = "cid:direct1"
	intake(t, s, c.Name, privateEvent, "k2")
	group, err := AudienceFor(ctx, s.DB, c.Name, "cid:group1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := AudienceSources(ctx, s.DB, group, 50)
	if err != nil {
		t.Fatal(err)
	}
	items := out.(map[string]any)["sources"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("group evidence mixed in another conversation: %+v", items)
	}
	recall := NormalizedEvent{Kind: EventRecall, Adapter: "dws", ParseVersion: "1", ProviderEventID: "evt-r",
		ProviderMessageID: "g1", ConversationID: "cid:group1", RecalledAt: "2026-09-14T12:00:00Z"}
	intake(t, s, c.Name, recall, "k3")
	if out, err = AudienceSources(ctx, s.DB, group, 50); err != nil {
		t.Fatal(err)
	}
	if len(out.(map[string]any)["sources"].([]map[string]any)) != 0 {
		t.Fatal("recalled message evidence still offered to the audience")
	}
}
