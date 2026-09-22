package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"encoding/json"
	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/console"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/observation"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
	runtimeengine "github.com/zhoushoujianwork/memgov/internal/runtime"
	localservice "github.com/zhoushoujianwork/memgov/internal/service"
)

type serviceWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *serviceWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func (a *app) requireStandalone() error {
	locked, err := localservice.Locked(a.home)
	if err != nil {
		return err
	}
	if locked {
		return core.Fail("conflict", "统一服务正在运行，请使用 service restart 或模块 pause/resume")
	}
	return nil
}
func (a *app) serviceCommand() *cobra.Command {
	root := &cobra.Command{Use: "service", Short: "统一管理采集、私聊、群聊和主动值守"}
	root.AddCommand(a.simple("status", "查看统一服务和系统托管状态", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) { return a.serviceStatus(ctx) }))
	root.AddCommand(a.simple("stop", "停止统一服务及所有模块", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
		stopCtx, cancel := context.WithTimeout(ctx, a.timeout)
		defer cancel()
		if manager, err := a.installedService(stopCtx); err != nil {
			return nil, err
		} else if manager != nil {
			if err := manager.Stop(stopCtx); err != nil {
				return nil, err
			}
		}
		if err := localservice.Stop(stopCtx, a.home); err != nil {
			return nil, err
		}
		return a.serviceStatus(ctx)
	}))
	root.AddCommand(a.simple("uninstall", "移除系统托管（保留数据与日志）", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
		manager, err := localservice.UserAgent(a.home)
		if err != nil {
			return nil, err
		}
		if err := manager.Uninstall(ctx); err != nil {
			return nil, err
		}
		return a.serviceStatus(ctx)
	}))
	for _, name := range []string{"start", "restart", "install", "run"} {
		name := name
		var noUI, open bool
		var port int
		cmd := &cobra.Command{Use: name, Short: "运行统一服务；安装系统托管后由系统管理", Hidden: name == "run", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if port < 0 || port > 65535 {
				return core.Fail("invalid_input", "--port must be 0..65535")
			}
			if name != "run" {
				ctx, cancel := context.WithTimeout(cmd.Context(), a.timeout)
				defer cancel()
				if name == "install" {
					return a.installService(ctx, noUI, port)
				}
				manager, err := a.installedService(ctx)
				if err != nil {
					return err
				}
				if manager != nil {
					for _, flag := range []string{"config", "no-ui", "port", "open"} {
						if cmd.Flags().Changed(flag) {
							return core.Fail("invalid_input", "managed settings are saved; use service install to change --%s", flag)
						}
					}
					before, err := localservice.Read(a.home)
					if err != nil {
						return err
					}
					previousID := ""
					if name == "restart" {
						previousID = before.ID
						if err := manager.Stop(ctx); err != nil {
							return err
						}
						if err := localservice.Stop(ctx, a.home); err != nil {
							return err
						}
					}
					if err := manager.Start(ctx); err != nil {
						return err
					}
					if _, err := localservice.WaitRunning(ctx, a.home, previousID); err != nil {
						return err
					}
					status, err := a.serviceStatus(ctx)
					if err != nil {
						return err
					}
					return a.emit(status, false)
				}
			}
			if name == "restart" {
				stopCtx, cancel := context.WithTimeout(cmd.Context(), a.timeout)
				defer cancel()
				if err := localservice.Stop(stopCtx, a.home); err != nil {
					return err
				}
			}
			return a.runUnified(cmd.Context(), noUI, port, open, name == "restart" || name == "run", name == "run")
		}}
		cmd.Flags().BoolVar(&noUI, "no-ui", false, "仅运行后台模块，不启动管理台")
		cmd.Flags().IntVar(&port, "port", 8787, "管理台端口，0 自动选择")
		cmd.Flags().BoolVar(&open, "open", false, "打开管理台浏览器")
		if name == "install" {
			cmd.Short = "安装并启动 macOS 系统托管，退出和程序更新后自动恢复"
		}
		root.AddCommand(cmd)
	}
	return root
}

func (a *app) installedService(ctx context.Context) (*localservice.LaunchAgent, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	m, err := localservice.UserAgent(a.home)
	if err != nil {
		return nil, err
	}
	if err := m.Inspect(ctx); err != nil {
		return nil, err
	}
	if !m.Installed && !m.Loaded {
		return nil, nil
	}
	return m, nil
}

func (a *app) serviceStatus(ctx context.Context) (any, error) {
	s, err := localservice.Read(a.home)
	if err != nil {
		return nil, err
	}
	m, err := a.installedService(ctx)
	if err != nil {
		return nil, err
	}
	return struct {
		localservice.Snapshot
		Manager *localservice.LaunchAgent `json:"manager,omitempty"`
	}{s, m}, nil
}

func (a *app) installService(ctx context.Context, noUI bool, port int) error {
	if err := a.requireCanonicalConfig("service install"); err != nil {
		return err
	}
	m, err := localservice.UserAgent(a.home)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	config, err := filepath.Abs(a.configPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(config); err != nil {
		return err
	}
	store, err := core.OpenReadCompatible(ctx, a.dbPath())
	if err != nil {
		return err
	}
	store.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	before, err := localservice.Read(a.home)
	if err != nil {
		return err
	}
	options := localservice.AgentOptions{Executable: executable, Home: a.home, Config: config, WorkingDirectory: cwd, SearchPath: os.Getenv("PATH"), UserHome: userHome, Port: port, NoUI: noUI}
	if err := m.Install(ctx, options); err != nil {
		return err
	}
	if _, err := localservice.WaitRunning(ctx, a.home, before.ID); err != nil {
		return err
	}
	status, err := a.serviceStatus(ctx)
	if err != nil {
		return err
	}
	return a.emit(status, false)
}

func (a *app) legacyActive(ctx context.Context) (bool, error) {
	s, err := core.Open(ctx, a.dbPath(), false)
	if err != nil {
		return false, err
	}
	defer s.Close()
	runtimes, err := core.RuntimeList(ctx, s.DB)
	if err != nil {
		return false, err
	}
	for _, r := range runtimes {
		if len(observation.Read(a.home, r.ID, time.Now())) > 0 {
			return true, nil
		}
	}
	channels, err := core.ChannelList(ctx, s.DB)
	if err != nil {
		return false, err
	}
	for _, c := range channels {
		lease, err := core.ReadLease(ctx, s.DB, c.ID)
		if err != nil {
			return false, err
		}
		if lease.Held {
			return true, nil
		}
	}
	return false, nil
}
func (a *app) stopLegacyModules(ctx context.Context) (returnErr error) {
	active, err := a.legacyActive(ctx)
	if err != nil || !active {
		return err
	}
	s, err := core.Open(ctx, a.dbPath(), false)
	if err != nil {
		return err
	}
	pausedRuntimes, pausedSources := []string{}, []string{}
	defer func() {
		if len(pausedRuntimes)+len(pausedSources) == 0 {
			return
		}
		restoreCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		store, e := core.Open(restoreCtx, a.dbPath(), false)
		if e != nil {
			if returnErr == nil {
				returnErr = e
			}
			return
		}
		defer store.Close()
		_, restoreErr := store.Mutate(restoreCtx, core.Request{Scope: "global", Command: "service.migrate.restore", Actor: a.actor}, func(tx *core.Tx) (any, error) {
			for _, id := range pausedRuntimes {
				if _, e := tx.SetRuntimeStatus(restoreCtx, id, "paused", ""); e != nil {
					return nil, e
				}
			}
			for _, id := range pausedSources {
				if _, e := tx.SetDataSourceStatus(restoreCtx, id, "paused"); e != nil {
					return nil, e
				}
			}
			return nil, nil
		})
		if restoreErr != nil && returnErr == nil {
			returnErr = restoreErr
		}
	}()
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "service.migrate", Actor: a.actor}, func(tx *core.Tx) (any, error) {
		runtimes, err := core.RuntimeList(ctx, tx.Conn)
		if err != nil {
			return nil, err
		}
		for _, r := range runtimes {
			if r.Status == "paused" {
				pausedRuntimes = append(pausedRuntimes, r.ID)
			}
			if _, err := tx.SetRuntimeStatus(ctx, r.ID, "stopped", ""); err != nil {
				return nil, err
			}
		}
		sources, err := core.DataSourceList(ctx, tx.Conn)
		if err != nil {
			return nil, err
		}
		for _, d := range sources {
			if d.Status == "paused" {
				pausedSources = append(pausedSources, d.ID)
			}
			if _, err := tx.SetDataSourceStatus(ctx, d.ID, "stopped"); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	s.Close()
	if err != nil {
		return err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		active, err := a.legacyActive(ctx)
		if err != nil || !active {
			return err
		}
		select {
		case <-ctx.Done():
			return core.Fail("unavailable", "旧模块尚未退出或释放租约，请稍后重试 service restart")
		case <-ticker.C:
		}
	}
}
func (a *app) runUnifiedOnce(ctx context.Context, noUI bool, port int, open, migrate, managed bool, restart func(context.Context, console.RestartRequest) (func(), error)) error {
	if err := a.requireCanonicalConfig("service run"); err != nil {
		return err
	}
	a.out = &serviceWriter{w: a.out}
	a.errOut = &serviceWriter{w: a.errOut}
	a.runtimeWake = runtimeengine.NewWakeBus()
	var s *core.Store
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	sup := localservice.Supervisor{Home: a.home, ConfigPath: a.configPath, Version: Version, Build: observation.ExecutableBuild(), Check: func(ctx context.Context) error {
		if managed {
			upgradeCtx, cancel := context.WithTimeout(ctx, a.timeout)
			backup, err := core.PrepareServiceDatabase(upgradeCtx, a.dbPath())
			cancel()
			if err != nil {
				return err
			}
			if backup != nil {
				_, _ = fmt.Fprintf(a.errOut, "服务数据库已升级：Schema %d → %d；升级前备份：%s\n", backup.SchemaVersion, core.SchemaVersion, backup.Path)
			}
		}
		var err error
		s, err = core.Open(ctx, a.dbPath(), false)
		if err != nil {
			return err
		}
		if migrate {
			migrationCtx, cancel := context.WithTimeout(ctx, a.timeout)
			defer cancel()
			if err := a.stopLegacyModules(migrationCtx); err != nil {
				return err
			}
		}
		active, err := a.legacyActive(ctx)
		if err != nil {
			return err
		}
		if active {
			return core.Fail("conflict", "旧采集或 Agent 仍在运行，请使用 service restart 迁移")
		}
		// Database startup failures retain actionable diagnostics in launchd.log.
		// Switch to redacted streaming errors before runtime modules can execute.
		a.streamMode = true
		return nil
	}, Select: func(ctx context.Context) ([]localservice.Spec, error) {
		specs, err := a.unifiedSpecs(ctx, s)
		if err != nil {
			return nil, err
		}
		if !noUI {
			specs = append(specs, localservice.Spec{Key: "console", Name: "本地管理台", Kind: "console", Run: func(ctx context.Context) error { return a.serveUIControlled(ctx, port, open, restart) }})
		}
		return specs, nil
	}}
	_, _ = fmt.Fprintln(a.errOut, "统一服务：采集、私聊、群聊与主动值守在同一进程运行；Ctrl-C 停止全部模块。")
	return sup.Run(ctx)
}
func (a *app) unifiedSpecs(ctx context.Context, s *core.Store) ([]localservice.Spec, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	applied, err := core.ReadAppliedConfig(ctx, tx, 0)
	if err != nil {
		return nil, err
	}
	var declaration DualModeDeclaration
	if applied.Version > 0 {
		if err := json.Unmarshal(applied.Declaration, &declaration); err != nil {
			return nil, err
		}
	}
	enabled := map[string]bool{}
	if p := declaration.Applications.Proactive; p != nil && p.Enabled != nil {
		enabled["proactive"] = *p.Enabled
	}
	for _, b := range ownerApplications(declaration.Applications) {
		enabled[b.Name] = b.App.Enabled != nil && *b.App.Enabled
	}
	for _, b := range groupApplications(declaration.Applications) {
		enabled[b.Name] = b.App.Enabled != nil && *b.App.Enabled
	}
	managed := map[string]bool{}
	for _, o := range applied.Objects {
		if o.ObjectType == "runtime" {
			managed[o.ObjectID] = enabled[o.Name]
		}
	}
	runtimes, err := core.RuntimeList(ctx, tx)
	if err != nil {
		return nil, err
	}
	sources, err := core.DataSourceList(ctx, tx)
	if err != nil {
		return nil, err
	}
	channels, err := core.ChannelList(ctx, tx)
	if err != nil {
		return nil, err
	}
	byChannel := map[string]core.Channel{}
	for _, c := range channels {
		byChannel[c.ID] = c
	}
	specs := []localservice.Spec{}
	collected := map[string]bool{}
	wanted := map[string]bool{}
	for _, d := range sources {
		if !d.Enabled {
			continue
		}
		d := d
		collected[d.ChannelID] = true
		specs = append(specs, localservice.Spec{Key: "source:" + d.ID, Name: d.Name, Kind: "source", Epoch: fmt.Sprintf("%d:%d", applied.Version, d.Version), Paused: d.Status == "paused", Resume: d.Status == "running", Run: func(ctx context.Context) error { return a.runDataSource(ctx, d.ID, true) }})
	}
	for _, r := range runtimes {
		if enabled, ok := managed[r.ID]; ok && !enabled {
			continue
		}
		if applied.Version > 0 {
			if _, ok := managed[r.ID]; !ok {
				continue
			}
		}
		r := r
		wanted[r.ChannelID] = true
		// Route discovery and control status update Version automatically. Applied
		// declaration changes restart workers; automatic routing must not do so.
		epoch := fmt.Sprint(applied.Version) + ":" + core.Digest(struct{ Channel, Profile, Analysis, Execution, Preset string }{r.ChannelID, r.ClaudeProfile, r.AnalysisModel, r.ExecutionModel, r.AgentPreset})
		specs = append(specs, localservice.Spec{Key: "agent:" + r.ID, Name: r.Name, Kind: r.ApplicationMode, Epoch: epoch, Paused: r.Status == "paused", Resume: r.Status == "running", Run: func(ctx context.Context) error { return a.runRuntimeWorker(ctx, r.ID, true) }})
	}
	for id := range wanted {
		if collected[id] {
			continue
		}
		c, ok := byChannel[id]
		if !ok {
			return nil, core.Fail("not_found", "module channel missing")
		}
		specs = append(specs, localservice.Spec{Key: "receiver:" + id, Name: c.Name, Kind: "receiver", Epoch: fmt.Sprint(applied.Version) + ":" + core.Digest(c), Run: func(ctx context.Context) error {
			store, err := core.Open(ctx, a.dbPath(), false)
			if err != nil {
				return err
			}
			defer store.Close()
			adapter, err := a.runtimeAdapter(ctx, store, c.ID)
			if err != nil {
				return err
			}
			logger, logErr := runlog.Open(a.home, c.ID, a.out, a.cfg.Logging.options())
			if logger != nil {
				defer logger.Close()
			}
			if logErr != nil {
				return logErr
			}
			receiver := runtimeengine.Service{Store: store, Adapter: adapter, DisableConfirmationCards: true, Logger: logger, Diagnostic: a.errOut, Notify: a.runtimeWake.Notify}
			return receiver.ReceiveChannel(ctx, c)
		}})
	}
	return specs, nil
}
