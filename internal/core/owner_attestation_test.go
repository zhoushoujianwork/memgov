package core

import (
	"context"
	"testing"
)

func TestAttestDWSOwnerPinsChannelAndOnlyExactUserID(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	var channel Channel
	_, err := s.Mutate(ctx, Request{Scope: "global", Command: "test.owner.channel"}, func(tx *Tx) (any, error) {
		var e error
		channel, e = tx.AddChannel(ctx, ChannelInput{Name: "owner-dws", Kind: ChannelDwsPersonal,
			Identity: ChannelIdentity{Profile: "corp:owner", ExpectedCorpID: "corp", ExpectedUserID: "owner"}})
		return nil, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "test.owner.stale"}, func(tx *Tx) (any, error) {
		return tx.AttestDWSOwner(ctx, channel.ID, channel.ConfigVersion+1)
	})
	if ErrorCode(err) != "conflict" {
		t.Fatalf("stale version: %v", err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global", Command: "test.owner.attest"}, func(tx *Tx) (any, error) {
		return tx.AttestDWSOwner(ctx, channel.ID, channel.ConfigVersion)
	})
	if err != nil {
		t.Fatal(err)
	}
	var verified int
	var basis string
	if err = s.DB.QueryRowContext(ctx, "SELECT verified,basis FROM identity_aliases WHERE tenant='corp' AND id_type='user_id' AND id_value='owner'").Scan(&verified, &basis); err != nil {
		t.Fatal(err)
	}
	if verified != 1 || basis != "authenticated_dws_profile" {
		t.Fatalf("attestation %d %q", verified, basis)
	}
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM identity_aliases WHERE tenant='corp' AND id_type='staff_id' AND id_value='owner'").Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if verified != 0 {
		t.Fatal("attestation must not infer staff_id from user_id")
	}
}
