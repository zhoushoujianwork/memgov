package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// publishTo permits disclosure of one memory version to one conversation, which
// is the precondition for citing it in a reply.

func openContext(t *testing.T, s *Store, channel string, in ContextInput) RequestContext {
	t.Helper()
	ctx := context.Background()
	var rc RequestContext
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "context.open", Key: "ctx|" + in.ConversationID + in.Query}, func(tx *Tx) (any, error) {
		var err error
		rc, err = tx.OpenContext(ctx, channel, in)
		return rc, err
	}); err != nil {
		t.Fatal(err)
	}
	return rc
}

func makeDraft(t *testing.T, s *Store, in DraftInput) (any, error) {
	t.Helper()
	ctx := context.Background()
	var out any
	// No idempotency key: a repeated call must reach Draft so the deduplication
	// under test is the outbox's own, not the request cache's.
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "outbox.draft"}, func(tx *Tx) (any, error) {
		var err error
		out, err = tx.Draft(ctx, in)
		return out, err
	})
	return out, err
}

func draftOf(t *testing.T, out any) Draft {
	t.Helper()
	d, ok := out.(map[string]any)["draft"].(Draft)
	if !ok {
		t.Fatalf("no draft in %+v", out)
	}
	return d
}

// A request context fixes the reply target from the trusted connection, so a
// model cannot claim a different sender, workspace or conversation later.
func TestRequestContextIsTrustedAndBounded(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	rc := openContext(t, s, c.Name, ContextInput{ConversationID: r.ConversationID,
		Sender: Sender{IDType: "union_id", IDValue: "alice"}, Query: "发布前要检查什么", TriggerMessage: "m1"})
	if rc.AudienceKey != r.AudienceKey || rc.RouteVersion != r.Version || rc.WorkspaceID != r.WorkspaceID {
		t.Fatalf("context did not inherit the route's audience: %+v", rc)
	}
	if rc.ExpiresAt <= rc.CreatedAt {
		t.Fatalf("a context must expire: %+v", rc)
	}
	// An unbound conversation has no route, so no context can be opened for it.
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "context.unbound"}, func(tx *Tx) (any, error) {
		return tx.OpenContext(ctx, c.Name, ContextInput{ConversationID: "cid:unbound", Sender: Sender{IDType: "union_id", IDValue: "alice"}})
	}); ErrorCode(err) != "denied" {
		t.Fatalf("a context was opened for an unbound conversation: %v", err)
	}
	// A display name is not an identity.
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "context.nameonly"}, func(tx *Tx) (any, error) {
		return tx.OpenContext(ctx, c.Name, ContextInput{ConversationID: r.ConversationID, Sender: Sender{DisplayName: "Alice"}})
	}); ErrorCode(err) != "invalid_input" {
		t.Fatalf("a context was opened from a display name: %v", err)
	}
	// An offline import may not claim a verified online trigger.
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "context.import"}, func(tx *Tx) (any, error) {
		return tx.OpenContext(ctx, c.Name, ContextInput{ConversationID: r.ConversationID, Origin: "import",
			Sender: Sender{IDType: "union_id", IDValue: "alice"}, TriggerMessage: "m1"})
	}); ErrorCode(err) != "denied" {
		t.Fatalf("an imported context forged a trigger: %v", err)
	}
}

// A draft may only cite what the conversation is allowed to see, whatever the
// model returned.
func TestDraftRefusesUndisclosableCitations(t *testing.T) {
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	rc := openContext(t, s, c.Name, ContextInput{ConversationID: r.ConversationID,
		Sender: Sender{IDType: "union_id", IDValue: "alice"}, Query: "发布前要检查什么"})
	_, err := makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: "先检查权限。", Citations: []string{"archived-memory"}})
	if ErrorCode(err) != "denied" {
		t.Fatalf("an unpublished memory was cited in a reply: %v", err)
	}
	out, err := makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: "先检查权限。"})
	if err != nil {
		t.Fatal(err)
	}
	d := draftOf(t, out)
	if d.State != "draft" || d.SendPolicy != "draft_only" {
		t.Fatalf("a new draft was not draft_only: %+v", d)
	}
	if out.(map[string]any)["sent"] != false {
		t.Fatalf("drafting claimed a send: %+v", out)
	}
	if d.SenderIdentity != c.AuthNamespace {
		t.Fatalf("the sending identity was not fixed by the channel: %+v", d)
	}
	// An empty reply is not a result, and an oversized one is refused.
	if _, err = makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: "  "}); ErrorCode(err) != "invalid_input" {
		t.Fatalf("an empty draft was accepted: %v", err)
	}
	if _, err = makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: strings.Repeat("字", maxDraftChars+1)}); ErrorCode(err) != "invalid_input" {
		t.Fatalf("an oversized draft was accepted: %v", err)
	}
	// The same reply task and input produce one draft, not two.
	again, err := makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: "先检查权限。"})
	if err != nil {
		t.Fatal(err)
	}
	if draftOf(t, again).ID != d.ID {
		t.Fatalf("the same input produced a second draft: %+v", again)
	}
}

// Previewing shows the whole display contract and never sends. All checks
// passing means the technical conditions hold, not that anyone approved.
func TestPreviewDisplaysWithoutSendingOrApproving(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	rc := openContext(t, s, c.Name, ContextInput{ConversationID: r.ConversationID,
		Sender: Sender{IDType: "union_id", IDValue: "alice"}, Query: "发布前要检查什么"})
	out, err := makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: "先检查权限，再看日志。"})
	if err != nil {
		t.Fatal(err)
	}
	d := draftOf(t, out)
	p, err := PreviewDraft(ctx, s.DB, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Approval["mode"] != "display_only" || p.Approval["status"] != "not_evaluated" {
		t.Fatalf("the approval contract changed: %+v", p.Approval)
	}
	if p.Target["conversation_id"] != r.ConversationID || p.Target["sender_identity"] != c.AuthNamespace {
		t.Fatalf("the target was not displayed: %+v", p.Target)
	}
	if len(p.Citations) != 0 {
		t.Fatalf("citations: %+v", p.Citations)
	}
	if !strings.Contains(p.Note, "sends nothing") {
		t.Fatalf("the note must say display does not send: %q", p.Note)
	}
	// The route is draft_only and the channel has no verified send capability, so
	// a technically complete draft is still not sendable.
	if p.Sendable {
		t.Fatalf("a draft_only route produced a sendable draft: %+v", p.Checks)
	}
	var policy, capability bool
	for _, check := range p.Checks {
		if check.Name == "send_policy" && !check.Passed {
			policy = true
		}
		if check.Name == "send_capability" && !check.Passed {
			capability = true
		}
	}
	if !policy && !capability {
		t.Fatalf("neither the policy nor the capability was reported as blocking: %+v", p.Checks)
	}
	// Previewing did not move the draft, and it created no delivery attempt.
	after, err := ReadDraft(ctx, s.DB, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "draft" {
		t.Fatalf("previewing changed the state to %q", after.State)
	}
	var attempts int
	if err = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM delivery_attempts WHERE outbox_id=?", d.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("previewing recorded %d delivery attempts", attempts)
	}
	// Dispatching is refused while the policy is draft_only, even with the right
	// digest, so display can never turn into a send.
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "outbox.dispatch"}, func(tx *Tx) (any, error) {
		return tx.AuthorizeDispatch(ctx, d.ID, p.DisplayDigest)
	}); ErrorCode(err) != "denied" {
		t.Fatalf("a draft_only draft was dispatched: %v", err)
	}
}

// Revoking a publication or withdrawing evidence makes an existing draft fail its
// checks, so a stale permission is never inherited at dispatch time.

func TestDraftRefusesWhenTheRouteChangedUnderIt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	rc := openContext(t, s, c.Name, ContextInput{ConversationID: r.ConversationID,
		Sender: Sender{IDType: "union_id", IDValue: "alice"}, Query: "发布前要检查什么"})
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.update"}, func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, r.ID, r.Version, RouteInput{AudiencePolicy: "conversation"}, "收窄到会话受众")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: "先检查权限。"}); ErrorCode(err) != "conflict" {
		t.Fatalf("a draft was produced against a stale route version: %v", err)
	}
}

// Delivery only becomes possible once sending was deliberately enabled and the
// capability was verified; an unknown outcome then stays unknown.
func TestDispatchRequiresEnabledSendingAndKeepsUnknownUnknown(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	// Sending is enabled explicitly on both the route and the channel.
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.enable.send"}, func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, r.ID, r.Version, RouteInput{ConversationID: r.ConversationID, SendPolicy: "dispatch_only"}, "本地操作者显式发送")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "probe"}, func(tx *Tx) (any, error) {
		return tx.SetChannelCapabilities(ctx, c.ID, Capabilities{Verified: map[string]bool{"send": true}}, "test")
	}); err != nil {
		t.Fatal(err)
	}
	rc := openContext(t, s, c.Name, ContextInput{ConversationID: r.ConversationID,
		Sender: Sender{IDType: "union_id", IDValue: "alice"}, Query: "发布前要检查什么", ReplyTransport: "dws_user"})
	out, err := makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: "先检查权限。"})
	if err != nil {
		t.Fatal(err)
	}
	d := draftOf(t, out)
	p, err := PreviewDraft(ctx, s.DB, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Sendable {
		t.Fatalf("an explicitly enabled draft was not sendable: %+v", p.Checks)
	}
	// A dispatch must bind the digest that was displayed.
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "outbox.dispatch.nodigest"}, func(tx *Tx) (any, error) {
		return tx.AuthorizeDispatch(ctx, d.ID, "")
	}); ErrorCode(err) != "invalid_input" {
		t.Fatalf("a dispatch without a digest was accepted: %v", err)
	}
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "outbox.dispatch.wrong"}, func(tx *Tx) (any, error) {
		return tx.AuthorizeDispatch(ctx, d.ID, "0000")
	}); ErrorCode(err) != "conflict" {
		t.Fatalf("a stale digest was accepted: %v", err)
	}
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "outbox.dispatch"}, func(tx *Tx) (any, error) {
		return tx.AuthorizeDispatch(ctx, d.ID, p.DisplayDigest)
	}); err != nil {
		t.Fatal(err)
	}
	sending, err := ReadDraft(ctx, s.DB, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sending.State != "sending" {
		t.Fatalf("authorizing a dispatch left state %q", sending.State)
	}
	// The attempt exists before the platform was contacted, so a crash here is
	// visible rather than silent.
	attempts, err := attemptsFor(ctx, s.DB, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].State != "sending" {
		t.Fatalf("attempts: %+v", attempts)
	}
	// accepted without a receipt is refused; the honest answer is unknown.
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "outbox.delivery.noreceipt"}, func(tx *Tx) (any, error) {
		return tx.RecordDelivery(ctx, d.ID, "accepted", "", "")
	}); ErrorCode(err) != "invalid_input" {
		t.Fatalf("accepted was recorded without a receipt: %v", err)
	}
	var result any
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "outbox.delivery"}, func(tx *Tx) (any, error) {
		result, err = tx.RecordDelivery(ctx, d.ID, "delivery_unknown", "", "request timed out after it was sent")
		return result, err
	}); err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["retry_safe"] != false {
		t.Fatalf("an unknown delivery was declared retry-safe: %+v", result)
	}
	// An unknown delivery is not cancellable, because a local cancel does not
	// recall a message that may already be out.
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "outbox.cancel"}, func(tx *Tx) (any, error) {
		return tx.CancelDraft(ctx, d.ID, "放弃")
	}); ErrorCode(err) != "conflict" {
		t.Fatalf("an unknown delivery was cancelled locally: %v", err)
	}
	// Reconciling requires the evidence that was actually checked.
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "outbox.reconcile.bare"}, func(tx *Tx) (any, error) {
		return tx.Reconcile(ctx, d.ID, "accepted", "receipt-1", "")
	}); ErrorCode(err) != "invalid_input" {
		t.Fatalf("a reconciliation without evidence was accepted: %v", err)
	}
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "outbox.reconcile"}, func(tx *Tx) (any, error) {
		return tx.Reconcile(ctx, d.ID, "accepted", "receipt-1", "会话中查到该条消息，messageId 一致")
	}); err != nil {
		t.Fatal(err)
	}
	settled, err := ReadDraft(ctx, s.DB, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != "accepted" || !strings.Contains(settled.Reason, "reconciled") {
		t.Fatalf("reconciliation was not recorded: %+v", settled)
	}
}

// An expired context cannot produce a late reply, because the conversation has
// moved on and the permission it captured is no longer current.
func TestExpiredContextCannotProduceAReply(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	rc := openContext(t, s, c.Name, ContextInput{ConversationID: r.ConversationID,
		Sender: Sender{IDType: "union_id", IDValue: "alice"}, Query: "发布前要检查什么"})
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := s.DB.ExecContext(ctx, "UPDATE request_contexts SET expires_at=? WHERE id=?", past, rc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: "迟到的回复"}); ErrorCode(err) != "denied" {
		t.Fatalf("an expired context produced a reply: %v", err)
	}
}
