package cli

import (
	"context"
	"encoding/json"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
	"io"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func (a *app) dataSourceCommands() {
	root := &cobra.Command{Use: "data-source", Short: "独立消息采集生命周期（与 AI 值守分开）"}
	root.AddCommand(a.historyImportCommand())
	root.AddCommand(a.dataSourceLogsCommand())
	root.AddCommand(a.dataSourceOwnerCommand())
	root.AddCommand(a.write("configure", "配置独立 DWS 数据源", cobra.NoArgs, func(ctx context.Context, tx *core.Tx, _ []string, raw json.RawMessage) (any, error) {
		var in core.DataSourceInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.ConfigureDataSource(ctx, in)
	}))
	root.AddCommand(a.read("list", "列出独立数据源", cobra.NoArgs, func(ctx context.Context, s *core.Store, _ []string) (any, error) {
		return core.DataSourceList(ctx, s.DB)
	}))
	root.AddCommand(a.read("status <source>", "查看采集状态、断点与覆盖", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		d, err := core.ReadDataSource(ctx, s.DB, args[0])
		if err != nil {
			return nil, err
		}
		coverage, err := core.DataSourceCoverageReport(ctx, s.DB, d)
		if err != nil {
			return nil, err
		}
		lease, err := core.ReadLease(ctx, s.DB, d.ChannelID)
		if err != nil {
			return nil, err
		}
		health, err := core.DataSourceLogHealth(ctx, s.DB, d.ID)
		if err != nil {
			return nil, err
		}
		discovery, discoveryErr := core.ReadSourceGroupDiscovery(ctx, s.DB, d.ID)
		if discoveryErr != nil && core.ErrorCode(discoveryErr) != "not_found" {
			return nil, discoveryErr
		}
		return map[string]any{"source": d, "coverage": coverage, "receiver_active": lease.Held, "logging": health, "discovery": discovery}, nil
	}))
	for _, item := range []struct{ name, status string }{{"pause", "paused"}, {"resume", "running"}, {"stop", "stopped"}} {
		item := item
		root.AddCommand(a.write(item.name+" <source>", "更新采集控制状态", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
			return tx.SetDataSourceStatus(ctx, args[0], item.status)
		}))
	}
	root.AddCommand(a.write("attach <source> <runtime>", "将已停止值守的采集职责移交到独立数据源", cobra.ExactArgs(2), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.BindRuntimeDataSource(ctx, args[1], args[0])
	}))
	root.AddCommand(&cobra.Command{Use: "start <source>", Short: "前台运行独立采集进程", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := a.requireStandalone(); err != nil {
			return err
		}
		return a.runDataSource(cmd.Context(), args[0], false)
	}})
	a.root.AddCommand(root)
}

func (a *app) runDataSource(ctx context.Context, value string, managed bool) error {
	s, err := core.Open(ctx, a.dbPath(), false)
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := core.ReadDataSource(ctx, s.DB, value)
	if err != nil {
		return err
	}
	if !managed {
		a.streamMode = true
		a.streamRuntimeID = d.ID
	}
	c, err := core.ReadChannel(ctx, s.DB, d.ChannelID)
	if err != nil {
		return err
	}
	adapter, err := a.adapterFor(c)
	if err != nil {
		return err
	}
	// Identity proof is acquired automatically at source startup. An
	// unavailable contact read leaves group Agent setup blocked while the
	// independent collector may still keep its durable cursor current.
	if _, attestErr := a.attestDataSourceOwner(ctx, s, d.ID); attestErr != nil {
		if core.ErrorCode(attestErr) == "denied" || core.ErrorCode(attestErr) == "conflict" {
			return attestErr
		}
		if a.errOut != nil {
			_, _ = io.WriteString(a.errOut, "owner identity attestation unavailable; group Agent setup remains blocked\n")
		}
	}
	worker := channel.HistoryImportWorker{Collector: channel.Collector{Store: s, Adapter: adapter}, Request: core.Request{Scope: d.WorkspaceID, Actor: "history-import"}}
	service := channel.DataSourceService{ExternalLifecycle: managed, Store: s, Adapter: adapter, HistoryStep: worker.Step, Diagnostic: a.errOut}
	logger, logErr := runlog.Open(a.home, d.ID, a.out, runlog.Options{})
	if logErr != nil {
		if a.errOut != nil {
			_, _ = io.WriteString(a.errOut, "data source logging degraded\n")
		}
		_, _ = s.Mutate(ctx, core.Request{Scope: "global", Command: "data-source.logging.degraded", Actor: a.actor}, func(tx *core.Tx) (any, error) { return nil, tx.SetDataSourceLogHealth(ctx, d.ID, true) })
	} else {
		defer logger.Close()
		service.Logger = logger
	}
	return service.Run(ctx, d.ID)
}
