package cli

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func (a *app) attestDataSourceOwner(ctx context.Context, s *core.Store, source string) (any, error) {
	d, err := core.ReadDataSource(ctx, s.DB, source)
	if err != nil {
		return nil, err
	}
	c, err := core.ReadChannel(ctx, s.DB, d.ChannelID)
	if err != nil {
		return nil, err
	}
	if c.Kind != core.ChannelDwsPersonal {
		return nil, core.Fail("invalid_input", "owner attestation requires a DWS source")
	}
	adapter, err := a.adapterFor(c)
	if err != nil {
		return nil, err
	}
	attestor, ok := adapter.(interface {
		AttestOwner(context.Context, core.Channel) error
	})
	if !ok {
		return nil, core.Fail("unavailable", "DWS adapter cannot attest owner identity")
	}
	if err = attestor.AttestOwner(ctx, c); err != nil {
		return nil, err
	}
	result, err := s.Mutate(ctx, core.Request{Scope: "global", Command: "data-source.attest-owner", Actor: a.actor,
		Input: map[string]any{"source_id": d.ID, "channel_id": c.ID, "channel_version": c.ConfigVersion}},
		func(tx *core.Tx) (any, error) { return tx.AttestDWSOwner(ctx, c.ID, c.ConfigVersion) })
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (a *app) dataSourceOwnerCommand() *cobra.Command {
	return a.simple("attest-owner <source>", "只读核验已登录 DWS 所有者并记录平台身份依据", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		s, err := core.Open(ctx, a.dbPath(), false)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return a.attestDataSourceOwner(ctx, s, args[0])
	})
}
