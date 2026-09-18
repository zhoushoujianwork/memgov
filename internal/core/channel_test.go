package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fixtureChannel registers a dws personal channel with one bound conversation so
// intake tests start from a realistic, fully configured route.
func fixtureChannel(t *testing.T, s *Store, kind, conversation string) (Channel, Route) {
	t.Helper()
	ctx := context.Background()
	in := ChannelInput{Name: "dws-main", Kind: kind, Identity: ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "user1"},
		CredentialRef: "keychain://memgov/dws-main", Route: &RouteInput{ConversationID: conversation, ConversationType: "group"}}
	if kind == ChannelDingTalkApp {
		in.Name = "app-bot"
		in.Identity = ChannelIdentity{ExpectedCorpID: "corp1", ClientID: "client1", RobotCode: "robot1"}
	}
	var c Channel
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "channel.add", Key: in.Name}, func(tx *Tx) (any, error) {
		var err error
		c, err = tx.AddChannel(ctx, in)
		return c, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Routes) != 1 {
		t.Fatalf("expected one route, got %d", len(c.Routes))
	}
	return c, c.Routes[0]
}

// A channel keeps personal and application identities apart, refuses inline
// secrets, and starts with no verified capability and no delivery permission.
func TestChannelIdentityAndDefaultsAreRestrictive(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	if c.AuthNamespace != "dws:corp1:user1" || c.IDNamespace != "dws:corp1" {
		t.Fatalf("namespaces %q %q", c.AuthNamespace, c.IDNamespace)
	}
	if r.SendPolicy != "draft_only" || r.ApprovalDisplay != "display_only" || r.AudiencePolicy != "local_private" {
		t.Fatalf("route defaults are not restrictive: %+v", r)
	}
	// The key is scoped to this exact route. A key shared by every private route
	// would turn one publication into a permission for every conversation.
	if r.AudienceKey != "local_private:"+c.ID+":"+r.ConversationID {
		t.Fatalf("audience key %q is not scoped to one conversation", r.AudienceKey)
	}
	if len(c.Capabilities.Verified) != 0 {
		t.Fatal("capabilities must start empty and be verified explicitly")
	}
	for _, bad := range []ChannelInput{
		{Name: "mixed", Kind: ChannelDwsPersonal, Identity: ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "u", RobotCode: "robot1"}},
		{Name: "app-no-client", Kind: ChannelDingTalkApp, Identity: ChannelIdentity{ExpectedCorpID: "corp1"}},
		{Name: "app-personal", Kind: ChannelDingTalkApp, Identity: ChannelIdentity{ExpectedCorpID: "corp1", ClientID: "c", ExpectedUserID: "u"}},
		{Name: "secret", Kind: ChannelDwsPersonal, Identity: ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "u"}, CredentialRef: "keychain://x?token=abc"},
		{Name: "plain", Kind: ChannelDwsPersonal, Identity: ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "u"}, CredentialRef: "AK123SECRET"},
		{Name: "Bad_Name", Kind: ChannelDwsPersonal, Identity: ChannelIdentity{ExpectedCorpID: "corp1", ExpectedUserID: "u"}},
		{Name: "wrong-kind", Kind: "telegram", Identity: ChannelIdentity{ExpectedCorpID: "corp1"}},
	} {
		_, err := s.Mutate(ctx, Request{Scope: "global", Command: "channel.add", Key: bad.Name}, func(tx *Tx) (any, error) {
			return tx.AddChannel(ctx, bad)
		})
		if ErrorCode(err) != "invalid_input" {
			t.Fatalf("channel %q accepted: %v", bad.Name, err)
		}
	}
	// An unbound conversation is refused rather than collected by default.
	if _, err := RouteFor(ctx, s.DB, c.ID, "cid:other"); ErrorCode(err) != "denied" {
		t.Fatalf("unbound conversation: %v", err)
	}
	plan, err := ChannelPlan(ctx, s.DB, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	if plan.(map[string]any)["creates_subscription"] != false || plan.(map[string]any)["sends_message"] != false {
		t.Fatal("plan must be offline and must not send")
	}
	report, err := ChannelDoctor(ctx, s.DB, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	if report.(map[string]any)["online_checked"] != false {
		t.Fatal("doctor must not contact the platform by default")
	}
	if report.(map[string]any)["healthy"] != false {
		t.Fatal("a channel with no verified capability is not healthy")
	}
}

func TestIgnoredRouteDropsMessageContent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:watched")
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.ignore"}, func(tx *Tx) (any, error) {
		return tx.AddRoute(ctx, c.ID, RouteInput{ConversationID: "cid:ignored", ConversationType: "group", Mode: "ignore"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var intake IntakeResult
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "message.ignore"}, func(tx *Tx) (any, error) {
		var intakeErr error
		intake, intakeErr = tx.Intake(ctx, c.ID, NormalizedEvent{Kind: EventMessage, Adapter: "test", ParseVersion: "test/1", Origin: "stream",
			ProviderMessageID: "ignored-message", ConversationID: "cid:ignored", Sender: Sender{IDType: "staff_id", IDValue: "someone"}, Body: "must not persist", SentAt: "2026-09-14T00:00:00Z"})
		return intake, intakeErr
	})
	if err != nil || intake.Status != "ignored" {
		t.Fatalf("ignored intake: %+v err=%v", intake, err)
	}
	var count int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE conversation_id='cid:ignored'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("ignored message content was persisted: count=%d err=%v", count, err)
	}
}

// Enabling delivery is a versioned change that invalidates drafts prepared under
// the previous policy, so an old draft cannot spend a new permission.
func TestRouteUpdateRequiresVersionAndStalesDrafts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO outbox(id,channel_id,route_id,route_version,conversation_id,audience_key,input_digest,state,created_at,updated_at) VALUES('o1',?,?,?,?,?,'d1','draft',?,?)",
		c.ID, r.ID, r.Version, r.ConversationID, r.AudienceKey, Now(), Now()); err != nil {
		t.Fatal(err)
	}
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.update"}, func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, r.ID, 99, RouteInput{SendPolicy: "dispatch_only"}, "enable sending")
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("stale expected version accepted: %v", err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "route.update.noreason"}, func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, r.ID, r.Version, RouteInput{SendPolicy: "dispatch_only"}, "")
	})
	if ErrorCode(err) != "invalid_input" {
		t.Fatalf("change without reason accepted: %v", err)
	}
	result, err := s.Mutate(ctx, Request{Scope: "global", Command: "route.update.ok"}, func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, r.ID, r.Version, RouteInput{SendPolicy: "dispatch_only", AudiencePolicy: "conversation"}, "enable sending")
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = result
	next, err := ReadRoute(ctx, s.DB, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next.Version != r.Version+1 || next.SendPolicy != "dispatch_only" {
		t.Fatalf("route not updated: %+v", next)
	}
	if next.AudienceKey != "conversation:"+c.ID+":"+r.ConversationID {
		t.Fatalf("audience key not recomputed: %q", next.AudienceKey)
	}
	var state, reason string
	if err = s.DB.QueryRowContext(ctx, "SELECT state,reason FROM outbox WHERE id='o1'").Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "stale" || !strings.Contains(reason, "route policy changed") {
		t.Fatalf("draft survived a policy change: %q %q", state, reason)
	}
	// A route may not be silently repointed at another conversation.
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "route.repoint"}, func(tx *Tx) (any, error) {
		return tx.UpdateRoute(ctx, r.ID, next.Version, RouteInput{ConversationID: "cid:other"}, "repoint")
	})
	if ErrorCode(err) != "invalid_input" {
		t.Fatalf("route repointed: %v", err)
	}
}

// Only one receiver holds a channel at a time, and a holder whose lease was
// taken over can no longer write because its fence is behind.
func TestChannelLeaseFencesStaleReceiver(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	var first Lease
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "lease.a"}, func(tx *Tx) (any, error) {
		var err error
		first, err = tx.AcquireLease(ctx, c.ID, "receiver-a", time.Hour)
		return first, err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "lease.b"}, func(tx *Tx) (any, error) {
		return tx.AcquireLease(ctx, c.ID, "receiver-b", time.Hour)
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("second receiver acquired a live lease: %v", err)
	}
	if err = CheckLease(ctx, s.DB, c.ID, first.Token, first.Fence); err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "lease.release"}, func(tx *Tx) (any, error) {
		return nil, tx.ReleaseLease(ctx, c.ID, first.Token)
	})
	if err != nil {
		t.Fatal(err)
	}
	var second Lease
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "lease.c"}, func(tx *Tx) (any, error) {
		var err error
		second, err = tx.AcquireLease(ctx, c.ID, "receiver-b", time.Hour)
		return second, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Fence <= first.Fence {
		t.Fatalf("fence did not advance: %d -> %d", first.Fence, second.Fence)
	}
	if err = CheckLease(ctx, s.DB, c.ID, first.Token, first.Fence); ErrorCode(err) != "conflict" {
		t.Fatalf("stale receiver may still write: %v", err)
	}
}
