package cli

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// historyImportCommand is mounted under data-source by its lifecycle commands.
func (a *app) historyImportCommand() *cobra.Command {
	root := &cobra.Command{Use: "history", Short: "可恢复的历史导入（只作上下文，不触发旧任务）"}
	var startAt, endAt, conversation string
	create := a.write("create <data-source>", "创建固定历史窗口，默认最近 30 天", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		source, err := core.ReadDataSource(ctx, tx.Conn, args[0])
		if err != nil {
			return nil, err
		}
		end := time.Now().UTC().Truncate(time.Second)
		if endAt != "" {
			end, err = time.Parse(time.RFC3339, endAt)
			if err != nil {
				return nil, core.Fail("invalid_input", "end must use RFC3339")
			}
		}
		start := end.Add(-30 * 24 * time.Hour)
		if startAt != "" {
			start, err = time.Parse(time.RFC3339, startAt)
			if err != nil {
				return nil, core.Fail("invalid_input", "start must use RFC3339")
			}
		}
		out := []core.HistoryImport{}
		for _, id := range source.RouteIDs {
			route, e := core.ReadRoute(ctx, tx.Conn, id)
			if e != nil {
				return nil, e
			}
			if conversation != "" && route.ConversationID != conversation {
				continue
			}
			if route.ConversationType != "group" || route.Status != "active" || route.Mode == "ignore" {
				continue
			}
			h, e := tx.CreateHistoryImport(ctx, source.ID, id, start, end)
			if e != nil {
				return nil, e
			}
			out = append(out, h)
		}
		if len(out) == 0 {
			return nil, core.Fail("not_found", "no active matching history conversation")
		}
		return out, nil
	})
	create.Flags().StringVar(&startAt, "start", "", "窗口起点 RFC3339")
	create.Flags().StringVar(&endAt, "end", "", "窗口终点 RFC3339")
	create.Flags().StringVar(&conversation, "conversation", "", "只导入指定已接管群；默认所有接管群")
	root.AddCommand(create)
	root.AddCommand(a.read("list <data-source>", "列出历史导入进度和安全错误码", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		source, err := core.ReadDataSource(ctx, s.DB, args[0])
		if err != nil {
			return nil, err
		}
		return core.ListHistoryImports(ctx, s.DB, source.ID)
	}))
	root.AddCommand(a.read("show <import-id>", "查看固定窗口、断点、去重计数和状态", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.ReadHistoryImport(ctx, s.DB, args[0])
	}))
	root.AddCommand(a.write("cancel <import-id>", "取消后续步骤并使在途结果失效", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.CancelHistoryImport(ctx, args[0])
	}))
	root.AddCommand(a.write("retry <import-id>", "从原断点重试失败或取消的导入", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.RetryHistoryImport(ctx, args[0])
	}))
	return root
}
