package core

import (
	"context"
	"strings"
	"testing"
)

func intake(t *testing.T, s *Store, channel string, e NormalizedEvent, key string) IntakeResult {
	t.Helper()
	ctx := context.Background()
	var out IntakeResult
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "message.intake", Key: key}, func(tx *Tx) (any, error) {
		var err error
		out, err = tx.Intake(ctx, channel, e)
		return out, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func intakeErr(t *testing.T, s *Store, channel string, e NormalizedEvent, key string) error {
	t.Helper()
	ctx := context.Background()
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "message.intake", Key: key}, func(tx *Tx) (any, error) {
		return tx.Intake(ctx, channel, e)
	})
	return err
}
func sampleEvent(providerID, body string, sender Sender) NormalizedEvent {
	return NormalizedEvent{Kind: EventMessage, Adapter: "dws", ParseVersion: "1", Origin: "stream",
		ProviderEventID: "evt-" + providerID, ProviderMessageID: providerID, ConversationID: "cid:group1",
		Sender: sender, Body: body, SentAt: "2026-09-14T10:00:00Z", EventAt: "2026-09-14T10:00:01Z"}
}

// A redelivered event writes nothing twice, while the same words from two people
// stay two separate messages with two separate evidence snapshots.
func TestIntakeDeduplicatesEventsAndNeverMergesDistinctSenders(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	alice := Sender{IDType: "union_id", IDValue: "alice", DisplayName: "同名"}
	bob := Sender{IDType: "union_id", IDValue: "bob", DisplayName: "同名"}
	first := intake(t, s, c.Name, sampleEvent("m1", "发布前先跑回归测试", alice), "k1")
	if first.Duplicate || first.MessageID == "" || first.Revision != 1 || first.SourceID == "" {
		t.Fatalf("first intake: %+v", first)
	}
	again := intake(t, s, c.Name, sampleEvent("m1", "发布前先跑回归测试", alice), "k2")
	if !again.Duplicate || again.MessageID != first.MessageID {
		t.Fatalf("redelivery was not idempotent: %+v", again)
	}
	other := intake(t, s, c.Name, sampleEvent("m2", "发布前先跑回归测试", bob), "k3")
	if other.MessageID == first.MessageID || other.SourceID == first.SourceID {
		t.Fatal("identical text from a different sender was merged")
	}
	var messages, sources int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sources WHERE kind='message'").Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if messages != 2 || sources != 2 {
		t.Fatalf("messages %d sources %d", messages, sources)
	}
	// A history page carries no event ID; re-reading it must not double-write.
	history := sampleEvent("m3", "灰度先放一台", alice)
	history.ProviderEventID, history.Origin = "", "history"
	if r := intake(t, s, c.Name, history, "k4"); r.Duplicate {
		t.Fatal("first history read was treated as duplicate")
	}
	if r := intake(t, s, c.Name, history, "k5"); !r.Duplicate {
		t.Fatal("re-reading the same history page wrote again")
	}
}

func TestMessageQuerySearchesObservedContentAndReportsCoverage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	e := sampleEvent("m-query", "请把旧机器切换到新资源", Sender{IDType: "union_id", IDValue: "peng", DisplayName: "彭伟"})
	e.SentAt = "2026-09-14T10:30:00Z"
	got := intake(t, s, c.Name, e, "query-intake")
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "message.observe", Key: "query-observe"}, func(tx *Tx) (any, error) {
		return nil, tx.ObserveMessage(ctx, c.ID, e.ConversationID, e.SentAt)
	}); err != nil {
		t.Fatal(err)
	}

	result, err := MessageQuery(ctx, s.DB, c.Name, MessageQueryInput{Query: "彭伟", Since: "2026-09-14T10:00:00+00:00", Until: "2026-09-14T11:00:00Z", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != got.MessageID || result.Messages[0].SenderDisplayName != "彭伟" || result.Messages[0].SourceID == "" {
		t.Fatalf("query did not return the current observed message with provenance: %+v", result)
	}
	if len(result.Coverage) != 1 || result.Coverage[0].ConversationType != "group" || result.Coverage[0].Watermark.ObservedAt != e.SentAt {
		t.Fatalf("query omitted the observation boundary: %+v", result.Coverage)
	}
	if result.CoverageSummary.ActiveConversations != 1 || result.CoverageSummary.ObservedConversations != 1 || result.CoverageSummary.UnresolvedGaps != 0 || result.CoverageSummary.ConversationTypes["group"] != 1 {
		t.Fatalf("query omitted the compact coverage summary: %+v", result.CoverageSummary)
	}
	if !strings.Contains(result.Note, "not formal memory") {
		t.Fatalf("query blurred observations and memory: %q", result.Note)
	}
	if _, err = MessageQuery(ctx, s.DB, c.Name, MessageQueryInput{Since: "2026-09-15T00:00:00Z", Until: "2026-09-14T00:00:00Z"}); ErrorCode(err) != "invalid_input" {
		t.Fatalf("invalid time range was accepted: %v", err)
	}

	recall := NormalizedEvent{Kind: EventRecall, Adapter: "dws", ParseVersion: "1", ProviderEventID: "evt-query-recall",
		ProviderMessageID: e.ProviderMessageID, ConversationID: e.ConversationID, RecalledAt: "2026-09-14T10:45:00Z"}
	intake(t, s, c.Name, recall, "query-recall")
	result, err = MessageQuery(ctx, s.DB, c.Name, MessageQueryInput{Query: "彭伟", Limit: 10})
	if err != nil || len(result.Messages) != 0 || len(result.Coverage) != 0 || result.CoverageSummary.ObservedConversations != 1 {
		t.Fatalf("recalled observation remained queryable or hid coverage: messages=%+v coverage=%+v error=%v", result.Messages, result.Coverage, err)
	}
}

// A cross-tenant event is refused, and two channels in different ID namespaces
// never collide on the same provider message ID.
func TestIntakeRejectsForeignTenantAndKeepsNamespacesApart(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	e := sampleEvent("m1", "内容", Sender{IDType: "union_id", IDValue: "alice"})
	e.Tenant = "corp2"
	if err := intakeErr(t, s, c.Name, e, "k1"); ErrorCode(err) != "denied" {
		t.Fatalf("foreign tenant accepted: %v", err)
	}
	var second Channel
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "channel.add", Key: "second"}, func(tx *Tx) (any, error) {
		var err error
		second, err = tx.AddChannel(ctx, ChannelInput{Name: "dws-other", Kind: ChannelDwsPersonal,
			Identity: ChannelIdentity{ExpectedCorpID: "corp2", ExpectedUserID: "user9"},
			Route:    &RouteInput{ConversationID: "cid:group1", ConversationType: "group"}})
		return second, err
	})
	if err != nil {
		t.Fatal(err)
	}
	a := intake(t, s, c.Name, sampleEvent("same-id", "第一租户", Sender{IDType: "union_id", IDValue: "alice"}), "k2")
	b := intake(t, s, second.Name, sampleEvent("same-id", "第二租户", Sender{IDType: "union_id", IDValue: "alice"}), "k3")
	if a.MessageID == b.MessageID || a.MessageKey == b.MessageKey {
		t.Fatalf("same provider ID collided across namespaces: %q %q", a.MessageKey, b.MessageKey)
	}
	// The same identifier in two tenants is two principals, never one.
	var principals int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM principals WHERE id_value='alice'").Scan(&principals); err != nil {
		t.Fatal(err)
	}
	if principals != 2 {
		t.Fatalf("expected one principal per tenant, got %d", principals)
	}
}

// An edit adds a revision without overwriting the original, an unchanged body
// only records another observation, and quotes to unknown parents are kept.
func TestIntakeVersionsEditsAndKeepsUnresolvedRelations(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	alice := Sender{IDType: "union_id", IDValue: "alice"}
	first := intake(t, s, c.Name, sampleEvent("m1", "原始内容", alice), "k1")
	same := sampleEvent("m1", "原始内容", alice)
	same.ProviderEventID = "evt-repeat"
	if r := intake(t, s, c.Name, same, "k2"); r.NewRevision || r.Revision != 1 {
		t.Fatalf("identical body created a revision: %+v", r)
	}
	edit := sampleEvent("m1", "修改后的内容", alice)
	edit.Kind, edit.ProviderEventID, edit.EditedAt = EventEdit, "evt-edit", "2026-09-14T10:05:00Z"
	edited := intake(t, s, c.Name, edit, "k3")
	if !edited.NewRevision || edited.Revision != 2 || edited.MessageID != first.MessageID {
		t.Fatalf("edit did not version the message: %+v", edited)
	}
	view, err := ReadMessage(ctx, s.DB, first.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Revisions) != 2 || view.Revisions[0].Body != "原始内容" {
		t.Fatalf("original revision was overwritten: %+v", view.Revisions)
	}
	if view.Revisions[0].SourceID == view.Revisions[1].SourceID {
		t.Fatal("each revision must own its evidence snapshot")
	}
	// A quote whose parent has not arrived is recorded unresolved and resolves
	// when the parent is later backfilled.
	quote := sampleEvent("m2", "引用一句", alice)
	quote.Relations = []Relation{{Kind: "quote", ProviderMessageID: "m-unknown"}}
	child := intake(t, s, c.Name, quote, "k4")
	var dst string
	if err = s.DB.QueryRowContext(ctx, "SELECT dst_message_id FROM message_relations WHERE src_message_id=?", child.MessageID).Scan(&dst); err != nil {
		t.Fatal(err)
	}
	if dst != "" {
		t.Fatal("unknown parent was invented")
	}
	parent := intake(t, s, c.Name, sampleEvent("m-unknown", "被引用的原文", alice), "k5")
	if err = s.DB.QueryRowContext(ctx, "SELECT dst_message_id FROM message_relations WHERE src_message_id=?", child.MessageID).Scan(&dst); err != nil {
		t.Fatal(err)
	}
	if dst != parent.MessageID {
		t.Fatalf("late backfill did not resolve the relation: %q", dst)
	}
}

// A recall withdraws the body and its evidence. A recall that arrives before the
// original keeps a later backfill unusable instead of exposing it.
func TestRecallWithdrawsEvidenceEvenWhenItArrivesFirst(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	alice := Sender{IDType: "union_id", IDValue: "alice"}
	first := intake(t, s, c.Name, sampleEvent("m1", "这句话稍后撤回", alice), "k1")
	recall := NormalizedEvent{Kind: EventRecall, Adapter: "dws", ParseVersion: "1", ProviderEventID: "evt-recall",
		ProviderMessageID: "m1", ConversationID: "cid:group1", RecalledAt: "2026-09-14T10:10:00Z"}
	got := intake(t, s, c.Name, recall, "k2")
	if got.Availability != "recalled" || got.MessageID != first.MessageID {
		t.Fatalf("recall not applied: %+v", got)
	}
	view, err := ReadMessage(ctx, s.DB, first.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Body != "" || view.Availability != "recalled" {
		t.Fatalf("recalled body still readable: %+v", view)
	}
	var state string
	var seq int
	if err = s.DB.QueryRowContext(ctx, "SELECT state,change_seq FROM source_availability WHERE source_id=?", first.SourceID).Scan(&state, &seq); err != nil {
		t.Fatal(err)
	}
	if state != "recalled" || seq < 2 {
		t.Fatalf("evidence not withdrawn: %q seq=%d", state, seq)
	}
	// Recall before the original: the later backfill must stay unusable.
	early := NormalizedEvent{Kind: EventRecall, Adapter: "dws", ParseVersion: "1", ProviderEventID: "evt-early",
		ProviderMessageID: "m9", ConversationID: "cid:group1", RecalledAt: "2026-09-14T11:00:00Z"}
	pending := intake(t, s, c.Name, early, "k3")
	if pending.Status != "pending_original" || pending.MessageID != "" {
		t.Fatalf("early recall: %+v", pending)
	}
	backfill := sampleEvent("m9", "撤回后才补齐的内容", alice)
	backfill.ProviderEventID, backfill.Origin = "", "history"
	late := intake(t, s, c.Name, backfill, "k4")
	if late.Availability != "recalled" {
		t.Fatalf("backfill after recall became usable: %+v", late)
	}
	lateView, err := ReadMessage(ctx, s.DB, late.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if lateView.Body != "" || lateView.Availability != "recalled" {
		t.Fatalf("recalled backfill is readable: %+v", lateView)
	}
	list, err := MessageList(ctx, s.DB, c.Name, "cid:group1", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Availability != "available" && m.Body != "" {
			t.Fatalf("listing exposed a recalled body: %+v", m)
		}
	}
}

// An offline import may not claim an online identity that was never verified,
// which is what stops a hand-written file from forging a principal.
func TestImportCannotForgeAnUnverifiedIdentity(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	forged := sampleEvent("m1", "冒充的内容", Sender{IDType: "union_id", IDValue: "boss"})
	forged.ProviderEventID, forged.Origin = "", "import"
	if err := intakeErr(t, s, c.Name, forged, "k1"); ErrorCode(err) != "denied" {
		t.Fatalf("import forged an identity: %v", err)
	}
	// An unresolved sender is allowed but stays weak, so it cannot authorize.
	unknown := sampleEvent("m2", "来源不明", Sender{IDType: "unknown", IDValue: "anon"})
	unknown.ProviderEventID, unknown.Origin = "", "import"
	out := intake(t, s, c.Name, unknown, "k2")
	view, err := ReadMessage(ctx, s.DB, out.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.WeakIdentity {
		t.Fatal("an unresolved sender must be marked weak")
	}
	var verified int
	if err = s.DB.QueryRowContext(ctx, "SELECT verified FROM identity_aliases WHERE id_value='anon'").Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if verified != 0 {
		t.Fatal("an unresolved alias must not be verified")
	}
	// Linking requires a verifiable basis; a display name is never one.
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "identity.link.bad"}, func(tx *Tx) (any, error) {
		return tx.LinkIdentity(ctx, c.Tenant, Sender{IDType: "user_id", IDValue: "u1"}, Sender{IDType: "union_id", IDValue: "alice"}, "same_display_name")
	})
	if ErrorCode(err) != "invalid_input" {
		t.Fatalf("display name accepted as a mapping basis: %v", err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "identity.link.ok"}, func(tx *Tx) (any, error) {
		return tx.LinkIdentity(ctx, c.Tenant, Sender{IDType: "user_id", IDValue: "u1"}, Sender{IDType: "union_id", IDValue: "alice"}, "platform_directory")
	})
	if err != nil {
		t.Fatal(err)
	}
	linked := sampleEvent("m3", "已验证身份的导入", Sender{IDType: "user_id", IDValue: "u1"})
	linked.ProviderEventID, linked.Origin = "", "import"
	if err = intakeErr(t, s, c.Name, linked, "k3"); err != nil {
		t.Fatalf("verified identity import refused: %v", err)
	}
}

// An unbound conversation is never collected, and inbox status makes an
// interrupted run visible instead of silently losing events.
func TestIntakeRefusesUnboundConversationAndReportsInboxStatus(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	stray := sampleEvent("m1", "另一个会话", Sender{IDType: "union_id", IDValue: "alice"})
	stray.ConversationID = "cid:unbound"
	if err := intakeErr(t, s, c.Name, stray, "k1"); ErrorCode(err) != "denied" {
		t.Fatalf("unbound conversation collected: %v", err)
	}
	var events int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM inbox_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatal("a refused event must not leave a partial row")
	}
	intake(t, s, c.Name, sampleEvent("m2", "正常消息", Sender{IDType: "union_id", IDValue: "alice"}), "k2")
	report, err := InboxList(ctx, s.DB, c.Name, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	counts := report.(map[string]any)["status_counts"].(map[string]int)
	if counts["applied"] != 1 {
		t.Fatalf("inbox status: %+v", counts)
	}
	for _, bad := range []NormalizedEvent{
		{Kind: "reaction", Adapter: "dws", ParseVersion: "1", ProviderMessageID: "m", ConversationID: "cid:group1"},
		{Kind: EventMessage, ParseVersion: "1", ProviderMessageID: "m", ConversationID: "cid:group1"},
		{Kind: EventMessage, Adapter: "dws", ParseVersion: "1", ProviderMessageID: "m", ConversationID: "cid:group1", Sender: Sender{DisplayName: "只有昵称"}},
		{Kind: EventMessage, Adapter: "dws", ParseVersion: "1", ProviderMessageID: "m", ConversationID: "cid:group1", Sender: Sender{IDType: "union_id", IDValue: "a"}, SentAt: "not-a-time"},
	} {
		if err = intakeErr(t, s, c.Name, bad, "bad-"+bad.Kind+bad.Adapter+bad.SentAt+bad.Sender.DisplayName); ErrorCode(err) != "invalid_input" {
			t.Fatalf("malformed event accepted: %+v -> %v", bad, err)
		}
	}
}

// A bot's own reply is marked self-authored so an assistant answer is not read
// back as a new user request, which is what would otherwise loop.
func TestSelfAuthoredMessagesAreMarked(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDingTalkApp, "cid:group1")
	own := sampleEvent("m1", "机器人自己的回复", Sender{IDType: "robot_code", IDValue: "robot1"})
	own.Adapter = "dingtalkapp"
	out := intake(t, s, c.Name, own, "k1")
	view, err := ReadMessage(ctx, s.DB, out.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.SelfAuthored {
		t.Fatal("the bot's own message must be marked self_authored")
	}
	human := sampleEvent("m2", "用户的提问", Sender{IDType: "union_id", IDValue: "alice"})
	human.Adapter = "dingtalkapp"
	other, err := ReadMessage(ctx, s.DB, intake(t, s, c.Name, human, "k2").MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if other.SelfAuthored {
		t.Fatal("a user message must not be marked self_authored")
	}
}

func TestAddressedMessagesAreRetainedForAgentRouting(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDingTalkApp, "cid:group1")
	group := sampleEvent("mentioned", "@机器人 请回答", Sender{IDType: "staff_id", IDValue: "alice"})
	group.Adapter, group.Mentioned = "dingtalkapp", true
	mentioned := intake(t, s, c.Name, group, "addressed-group")

	var addressed int
	if err := s.DB.QueryRowContext(ctx, "SELECT addressed FROM messages WHERE id=?", mentioned.MessageID).Scan(&addressed); err != nil || addressed != 1 {
		t.Fatalf("group mention addressed=%d err=%v", addressed, err)
	}
	var directRoute Route
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.direct"}, func(tx *Tx) (any, error) {
		var addErr error
		directRoute, addErr = tx.AddRoute(ctx, c.ID, RouteInput{ConversationID: "cid:direct", ConversationType: "direct"})
		return directRoute, addErr
	})
	if err != nil {
		t.Fatal(err)
	}
	direct := sampleEvent("direct", "你好", Sender{IDType: "staff_id", IDValue: "alice"})
	direct.Adapter, direct.ConversationID = "dingtalkapp", "cid:direct"
	directMessage := intake(t, s, c.Name, direct, "addressed-direct")
	if err := s.DB.QueryRowContext(ctx, "SELECT addressed FROM messages WHERE id=?", directMessage.MessageID).Scan(&addressed); err != nil || addressed != 1 {
		t.Fatalf("direct message addressed=%d err=%v", addressed, err)
	}
}

// The canonical snapshot carries message identity, so evidence text alone can
// never be confused between two senders.
func TestMessageSnapshotBindsIdentity(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	out := intake(t, s, c.Name, sampleEvent("m1", "证据正文", Sender{IDType: "union_id", IDValue: "alice"}), "k1")
	var content string
	if err := s.DB.QueryRowContext(ctx, "SELECT content FROM sources WHERE id=?", out.SourceID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"message_key: " + out.MessageKey, "sender: union_id:alice", "revision: 1", "证据正文"} {
		if !strings.Contains(content, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, content)
		}
	}
}
