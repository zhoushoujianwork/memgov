package cli

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/agent"
)

func (a *app) agentCommands() {
	root := &cobra.Command{Use: "agent", Short: "受控 Agent preset"}
	preset := &cobra.Command{Use: "preset", Short: "Agent 规则目录生命周期"}
	var name string
	enable := a.simple("enable <harness>", "Create a versioned Agent harness preset", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		presetName := name
		if presetName == "" {
			presetName = args[0] + "-default"
		}
		return agent.Enable(ctx, a.home, args[0], presetName)
	})
	enable.Flags().StringVar(&name, "name", "", "Preset name; defaults to <harness>-default")
	preset.AddCommand(enable)
	preset.AddCommand(a.simple("status <name>", "检查 preset commit 与干净状态", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		return agent.Status(ctx, a.home, args[0])
	}))
	preset.AddCommand(a.simple("disable <name>", "禁用 preset，保留目录和 Git 历史", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		return agent.Disable(ctx, a.home, args[0])
	}))
	var source, claudeSource string
	sync := a.simple("sync <name>", "Import and commit a controlled policy copy", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		if claudeSource != "" {
			return agent.SyncClaudeMD(ctx, a.home, args[0], claudeSource)
		}
		return agent.SyncPolicy(ctx, a.home, args[0], source)
	})
	sync.Flags().StringVar(&source, "from-policy", "", "Policy file to import for this harness")
	sync.Flags().StringVar(&claudeSource, "from-claude-md", "", "Compatibility import for a Claude preset")
	sync.MarkFlagsMutuallyExclusive("from-policy", "from-claude-md")
	preset.AddCommand(sync)
	root.AddCommand(preset)
	a.root.AddCommand(root)
}
