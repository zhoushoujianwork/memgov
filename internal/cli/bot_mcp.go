package cli

import (
	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/runtime"
)

func (a *app) botMCPCommand() {
	var taskID, attemptID string
	cmd := &cobra.Command{Use: "bot-mcp", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return runtime.ServeBotMCP(cmd.Context(), a.home, taskID, attemptID, a.in, a.out)
	}}
	cmd.Flags().StringVar(&taskID, "task-id", "", "Claimed group task")
	cmd.Flags().StringVar(&attemptID, "attempt-id", "", "Claimed group attempt")
	a.root.AddCommand(cmd)
}
