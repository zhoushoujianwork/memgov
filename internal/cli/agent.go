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
	enable := a.simple("enable claude", "创建带 Git 审计的 Claude preset", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		return agent.Enable(ctx, a.home, args[0], name)
	})
	enable.Flags().StringVar(&name, "name", "claude-default", "preset 名称")
	preset.AddCommand(enable)
	preset.AddCommand(a.simple("status <name>", "检查 preset commit 与干净状态", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		return agent.Status(ctx, a.home, args[0])
	}))
	preset.AddCommand(a.simple("disable <name>", "禁用 preset，保留目录和 Git 历史", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		return agent.Disable(ctx, a.home, args[0])
	}))
	var source string
	sync := a.simple("sync <name>", "导入 CLAUDE.md 的受控副本并提交", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		return agent.SyncClaudeMD(ctx, a.home, args[0], source)
	})
	sync.Flags().StringVar(&source, "from-claude-md", "", "要导入的普通文件（必填）")
	preset.AddCommand(sync)
	root.AddCommand(preset)
	a.root.AddCommand(root)
}
