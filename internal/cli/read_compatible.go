package cli

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// readCompatible is reserved for commands whose query only uses the v1 core
// schema. It permits inspection while a newer binary is available before an
// explicit database schema upgrade is performed.
func (a *app) readCompatible(use, short string, args cobra.PositionalArgs, fn func(context.Context, *core.Store, []string) (any, error)) *cobra.Command {
	return a.simple(use, short, args, func(ctx context.Context, values []string) (any, error) {
		s, err := core.OpenReadCompatible(ctx, a.dbPath())
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return fn(ctx, s, values)
	})
}
