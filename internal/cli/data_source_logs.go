package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

func (a *app) dataSourceLogsCommand() *cobra.Command {
	root := &cobra.Command{Use: "logs", Short: "独立采集日志"}
	sourceID := func(ctx context.Context, value string) (string, error) {
		s, err := core.Open(ctx, a.dbPath(), false)
		if err != nil {
			return "", err
		}
		defer s.Close()
		d, err := core.ReadDataSource(ctx, s.DB, value)
		return d.ID, err
	}
	root.AddCommand(a.simple("list <source>", "列出采集 JSONL 日志", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		id, err := sourceID(ctx, args[0])
		if err != nil {
			return nil, err
		}
		return runlog.Files(a.home, id)
	}))
	var since, until, level, component string
	var limit int
	filter := func() (runlog.Filter, error) {
		f := runlog.Filter{Level: level, Component: component, Limit: limit}
		var err error
		if since != "" {
			f.Since, err = time.Parse(time.RFC3339, since)
			if err != nil {
				return f, core.Fail("invalid_input", "--since must use RFC3339")
			}
		}
		if until != "" {
			f.Until, err = time.Parse(time.RFC3339, until)
			if err != nil {
				return f, core.Fail("invalid_input", "--until must use RFC3339")
			}
		}
		return f, nil
	}
	show := a.simple("show <source>", "读取安全摘要与耗时", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		id, err := sourceID(ctx, args[0])
		if err != nil {
			return nil, err
		}
		f, err := filter()
		if err != nil {
			return nil, err
		}
		return runlog.Show(a.home, id, f)
	})
	follow := &cobra.Command{Use: "follow <source>", Short: "持续输出采集 NDJSON 日志", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := sourceID(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		f, err := filter()
		if err != nil {
			return err
		}
		a.streamMode = true
		a.streamRuntimeID = id
		return runlog.Follow(cmd.Context(), a.home, id, f, a.out)
	}}
	for _, cmd := range []*cobra.Command{show, follow} {
		cmd.Flags().StringVar(&since, "since", "", "起始时间 RFC3339")
		cmd.Flags().StringVar(&until, "until", "", "结束时间 RFC3339")
		cmd.Flags().StringVar(&level, "level", "", "日志级别")
		cmd.Flags().StringVar(&component, "component", "", "组件")
		cmd.Flags().IntVar(&limit, "limit", 100, "返回条数")
	}
	root.AddCommand(show, follow)
	return root
}
