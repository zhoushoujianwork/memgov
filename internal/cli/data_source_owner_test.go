package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type ownerStubAdapter struct {
	*stubAdapter
	attestErr error
}

func (a *ownerStubAdapter) AttestOwner(context.Context, core.Channel) error { return a.attestErr }

func TestDataSourceAttestOwnerPersistsOnlyVerifiedRead(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	s, err := core.Open(ctx, filepath.Join(home, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "test.source"}, func(tx *core.Tx) (any, error) {
		c, e := tx.AddChannel(ctx, core.ChannelInput{Name: "dws-owner", Kind: core.ChannelDwsPersonal,
			Identity: core.ChannelIdentity{ExpectedCorpID: "corp", ExpectedUserID: "owner", Profile: "corp:owner"}})
		if e != nil {
			return nil, e
		}
		return tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "work-chat", Channel: c.ID, Workspace: "global"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	fake := &ownerStubAdapter{stubAdapter: &stubAdapter{}}
	fake.attestErr = core.Fail("denied", "profile mismatch")
	if code, _ := invokeWith(t, fake, home, "", "data-source", "attest-owner", "work-chat"); code == 0 {
		t.Fatal("mismatched profile must block attestation")
	}
	fake.attestErr = nil
	if code, value := invokeWith(t, fake, home, "", "data-source", "attest-owner", "work-chat"); code != 0 {
		t.Fatalf("attest failed: %+v", value)
	}
	s, err = core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var verified int
	if err = s.DB.QueryRowContext(ctx, "SELECT verified FROM identity_aliases WHERE tenant='corp' AND id_type='user_id' AND id_value='owner'").Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if verified != 1 {
		t.Fatal("owner user_id was not verified")
	}
}
