package cli

import (
	"context"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/knowledge"
)

func (a *app) knowledgeCommands() {
	knowledgeCommand := &cobra.Command{Use: "knowledge", Short: "Owner knowledge source credentials"}
	knowledgeCommand.AddCommand(a.simple("status", "Show configured sources without secrets", cobra.NoArgs, func(context.Context, []string) (any, error) {
		return knowledge.Status(a.home)
	}))
	var source string
	importCommand := a.simple("import-relayer", "Copy legacy source credentials into memgov", cobra.NoArgs, func(context.Context, []string) (any, error) {
		if source == "" {
			return nil, core.Fail("invalid_input", "--from legacy credential database is required")
		}
		return knowledge.ImportRelayer(a.home, source)
	})
	importCommand.Flags().StringVar(&source, "from", "", "Absolute path to legacy external-knowledge.sqlite")
	knowledgeCommand.AddCommand(importCommand)
	knowledgeCommand.AddCommand(a.simple("configure", "Replace both source credentials from --input JSON", cobra.NoArgs, func(context.Context, []string) (any, error) {
		if a.input == "" {
			return nil, core.Fail("invalid_input", "--input JSON file or - is required")
		}
		raw, err := a.payload()
		if err != nil {
			return nil, err
		}
		return knowledge.Configure(a.home, raw)
	}))
	a.root.AddCommand(knowledgeCommand)
	var sources string
	mcp := &cobra.Command{Use: "knowledge-mcp", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if sources == "" {
			return core.Fail("invalid_input", "--sources is required")
		}
		return knowledge.ServeMCP(cmd.Context(), a.home, strings.Split(sources, ","), a.in, a.out)
	}}
	mcp.Flags().StringVar(&sources, "sources", "", "Comma-separated read-only sources")
	a.root.AddCommand(mcp)
}
