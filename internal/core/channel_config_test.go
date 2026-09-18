package core

import (
	"context"
	"testing"
	"time"
)

func TestChannelConfigIdentityLeaseAndAuditRollback(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, route := fixtureChannel(t, s, ChannelDingTalkApp, "cid:group1")
	in := ChannelInput{Name: c.Name, Kind: c.Kind, Identity: c.Identity, CredentialRef: "env://ROTATED_SECRET"}
	apply := func(in ChannelInput, expected int) error {
		_, err := s.Mutate(ctx, Request{Scope: "global", Command: "config.apply"}, func(tx *Tx) (any, error) {
			return tx.ApplyChannelConfig(ctx, in, expected, 0, "rotate credential")
		})
		return err
	}
	other := in
	other.Identity.ClientID = "other-app"
	if err := apply(other, 1); ErrorCode(err) != "conflict" {
		t.Fatalf("identity changed: %v", err)
	}
	var lease Lease
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "lease"}, func(tx *Tx) (any, error) {
		var err error
		lease, err = tx.AcquireLease(ctx, c.ID, "receiver", time.Minute)
		return lease, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(in, 1); ErrorCode(err) != "conflict" {
		t.Fatalf("live receiver config changed: %v", err)
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE channel_leases SET until='2000-01-01T00:00:00Z' WHERE channel_id=?", c.ID); err != nil {
		t.Fatal(err)
	}
	// A late audit failure must roll back both credential and version changes.
	if _, err = s.DB.ExecContext(ctx, `CREATE TRIGGER fail_channel_audit BEFORE INSERT ON operations BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := apply(in, 1); err == nil {
		t.Fatal("audit failure ignored")
	}
	stored, err := ReadChannel(ctx, s.DB, c.ID)
	if err != nil || stored.ConfigVersion != 1 || stored.CredentialRef != c.CredentialRef {
		t.Fatalf("partial update: %+v %v", stored, err)
	}
	if _, err = s.DB.ExecContext(ctx, "DROP TRIGGER fail_channel_audit"); err != nil {
		t.Fatal(err)
	}
	if err := apply(in, 1); err != nil {
		t.Fatal(err)
	}
	stored, err = ReadChannel(ctx, s.DB, c.ID)
	if err != nil || stored.ConfigVersion != 2 || stored.CredentialRef != in.CredentialRef || len(stored.Routes) != 1 || stored.Routes[0].Version != route.Version {
		t.Fatalf("credential-only update changed routes: %+v %v", stored, err)
	}
}

func TestChannelConfigInvalidatesCapabilitiesAndDrafts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "probe"}, func(tx *Tx) (any, error) {
		return tx.SetChannelCapabilities(ctx, c.ID, Capabilities{Verified: map[string]bool{"send": true}}, "fake")
	})
	if err != nil {
		t.Fatal(err)
	}
	// A draft with no citations still binds the sender and route configuration.
	rc := openContext(t, s, c.Name, ContextInput{ConversationID: r.ConversationID, Sender: Sender{IDType: "staff_id", IDValue: "user1"}})
	out, err := makeDraft(t, s, DraftInput{ContextID: rc.ID, Content: "test draft"})
	if err != nil {
		t.Fatal(err)
	}
	draft := draftOf(t, out)
	in := ChannelInput{Name: c.Name, Kind: c.Kind, Identity: c.Identity}
	in.Identity.DeliveryRobotCode = "new-robot"
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "config.apply"}, func(tx *Tx) (any, error) {
		return tx.ApplyChannelConfig(ctx, in, 2, 0, "replace delivery robot")
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := ReadChannel(ctx, s.DB, c.ID)
	if err != nil || len(stored.Capabilities.Verified) != 0 || stored.ConfigVersion != 3 {
		t.Fatalf("stale capabilities: %+v %v", stored, err)
	}
	draft, err = ReadDraft(ctx, s.DB, draft.ID)
	if err != nil || draft.State != "stale" {
		t.Fatalf("old draft remains usable: %+v %v", draft, err)
	}
}
