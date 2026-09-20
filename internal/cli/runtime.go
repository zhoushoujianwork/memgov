package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/channel/dws"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/observation"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
	runtimeengine "github.com/zhoushoujianwork/memgov/internal/runtime"
)

func (a *app) runtimeCommands() {
	root := &cobra.Command{Use: "runtime", Short: "钉钉 AI 值守运行时"}
	root.AddCommand(a.runtimeMessageCmd())
	root.AddCommand(a.runtimeSetupCommand())
	root.AddCommand(a.simple("harness [name]", "查看已注册 Agent harness 及其运行契约", cobra.MaximumNArgs(1), func(_ context.Context, args []string) (any, error) {
		// Diagnostics intentionally use the same registry and factory path as
		// runtime startup. They do not open the database or invoke a model, so
		// this command remains useful before the first runtime is configured.
		if len(args) == 1 {
			return runtimeengine.DiagnoseHarness(args[0], "", ""), nil
		}
		return runtimeengine.DiagnoseHarnesses("", ""), nil
	}))
	root.AddCommand(a.write("configure", "创建或更新值守配置", cobra.NoArgs, func(ctx context.Context, tx *core.Tx, _ []string, raw json.RawMessage) (any, error) {
		var in core.RuntimeConfigInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if in.ExpectedVersion == 0 {
			in.ExpectedVersion = a.expected
		}
		return tx.ConfigureRuntime(ctx, in)
	}))
	root.AddCommand(a.simple("status [runtime]", "查看值守状态", cobra.MaximumNArgs(1), func(ctx context.Context, args []string) (any, error) {
		s, err := core.Open(ctx, a.dbPath(), false)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if len(args) == 0 {
			return core.RuntimeList(ctx, s.DB)
		}
		return core.RuntimeStatusFor(ctx, s.DB, args[0])
	}))
	for _, transition := range []struct {
		name, status, help string
	}{
		{"pause", "paused", "暂停新分析和执行，继续采集"},
		{"resume", "running", "恢复分析和执行"},
		{"stop", "stopped", "停止值守进程"},
	} {
		item := transition
		root.AddCommand(a.write(item.name+" <runtime>", item.help, cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
			return tx.SetRuntimeStatus(ctx, args[0], item.status, "")
		}))
	}

	root.AddCommand(&cobra.Command{
		Use:   "start <runtime>",
		Short: "前台运行值守并输出 JSONL 日志",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireStandalone(); err != nil {
				return err
			}
			return a.runRuntime(cmd.Context(), args[0])
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "restart <runtime>",
		Short: "停止旧进程并在当前终端重新运行值守",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireStandalone(); err != nil {
				return err
			}
			return a.restartRuntime(cmd.Context(), args[0])
		},
	})

	task := &cobra.Command{Use: "task", Short: "值守任务"}
	var status string
	var limit int
	list := a.read("list <runtime>", "列出值守任务", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.RuntimeTaskList(ctx, s.DB, args[0], status, limit)
	})
	list.Flags().StringVar(&status, "status", "", "按任务状态过滤")
	list.Flags().IntVar(&limit, "limit", 50, "返回数量，1—500")
	task.AddCommand(list)
	task.AddCommand(a.write("propose-action <task-id>", "为当前 Agent 会话准备待本人确认的操作", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, raw json.RawMessage) (any, error) {
		var in core.RuntimeActionProposal
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.ProposeRuntimeAction(ctx, args[0], in)
	}))
	task.AddCommand(a.write("capture-hotword <task-id>", "保存本人明确纠正的语音转写热词", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, raw json.RawMessage) (any, error) {
		var in core.HotwordCaptureInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.CaptureRuntimeHotword(ctx, args[0], in)
	}))
	task.AddCommand(a.read("show <task-id>", "读取任务、来源、执行和待确认动作", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.ReadRuntimeTask(ctx, s.DB, args[0])
	}))
	task.AddCommand(a.write("cancel <task-id>", "取消未完成任务并作废确认动作", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.SetRuntimeTaskStatus(ctx, args[0], "cancelled")
	}))
	task.AddCommand(a.write("retry <task-id>", "重试失败、过期或已取消任务", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.SetRuntimeTaskStatus(ctx, args[0], "pending")
	}))
	task.AddCommand(a.write("resume <task-id>", "恢复失败任务的原会话或原进度，并发送继续指令", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.ResumeRuntimeTask(ctx, args[0], a.expected)
	}))
	task.AddCommand(a.write("confirm <action-id>", "验证并接受本人钉钉私聊中的具体操作确认", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, raw json.RawMessage) (any, error) {
		var in struct {
			MessageID string `json:"message_id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.ConfirmRuntimeActionFromMessage(ctx, args[0], in.MessageID)
	}))
	root.AddCommand(task)

	logs := &cobra.Command{Use: "logs", Short: "独立运行日志"}
	logs.AddCommand(a.simple("list <runtime>", "列出滚动 JSONL 文件", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		cfg, err := runtimeConfig(ctx, a, args[0])
		if err != nil {
			return nil, err
		}
		return runlog.Files(a.home, cfg.ID)
	}))
	var since, until, level, component, batchID, taskID string
	var logLimit int
	filter := func() (runlog.Filter, error) {
		f := runlog.Filter{Level: level, Component: component, BatchID: batchID, TaskID: taskID, Limit: logLimit}
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
	show := a.simple("show <runtime>", "查询并过滤结构化日志", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		cfg, err := runtimeConfig(ctx, a, args[0])
		if err != nil {
			return nil, err
		}
		f, err := filter()
		if err != nil {
			return nil, err
		}
		events, err := runlog.Show(a.home, cfg.ID, f)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"events": events}
		if taskID != "" {
			s, openErr := core.Open(ctx, a.dbPath(), false)
			if openErr != nil {
				return nil, openErr
			}
			defer s.Close()
			task, readErr := core.ReadRuntimeTask(ctx, s.DB, taskID)
			if readErr != nil {
				return nil, readErr
			}
			if task.RuntimeID != cfg.ID {
				return nil, core.Fail("denied", "task does not belong to this runtime")
			}
			result["task"] = task
		}
		return result, nil
	})
	addLogFlags(show, &since, &until, &level, &component, &batchID, &taskID, &logLimit)
	logs.AddCommand(show)
	follow := &cobra.Command{
		Use:   "follow <runtime>",
		Short: "持续输出匹配的 JSONL 日志",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := runtimeConfig(cmd.Context(), a, args[0])
			if err != nil {
				return err
			}
			f, err := filter()
			if err != nil {
				return err
			}
			a.streamMode = true
			a.streamRuntimeID = cfg.ID
			return runlog.Follow(cmd.Context(), a.home, cfg.ID, f, a.out)
		},
	}
	addLogFlags(follow, &since, &until, &level, &component, &batchID, &taskID, &logLimit)
	logs.AddCommand(follow)
	root.AddCommand(logs)
	a.root.AddCommand(root)
}

func (a *app) runRuntime(ctx context.Context, value string) error {
	return a.runRuntimeWorker(ctx, value, false)
}

func (a *app) runRuntimeWorker(ctx context.Context, value string, externalReceiver bool) error {
	s, err := core.Open(ctx, a.dbPath(), false)
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, err := core.ReadRuntime(ctx, s.DB, value)
	if err != nil {
		return err
	}
	if !externalReceiver {
		a.streamMode = true
		a.streamRuntimeID = cfg.ID
	}
	adapter, err := a.runtimeAdapter(ctx, s, cfg.ChannelID)
	if err != nil {
		return err
	}
	logger, logErr := runlog.Open(a.home, cfg.ID, a.out, a.cfg.Logging.options())
	if logger != nil {
		defer logger.Close()
	}
	harnessName := "claude"
	if cfg.AgentPreset != "" {
		preset, presetErr := agent.Status(ctx, a.home, cfg.AgentPreset)
		if presetErr != nil {
			return presetErr
		}
		harnessName = preset.Provider
	}
	bundle, harnessErr := runtimeengine.NewHarness(harnessName, cfg.AnalysisModel, cfg.ExecutionModel)
	if harnessErr != nil {
		return harnessErr
	}
	if profiled, ok := bundle.Executor.(interface{ SetProfile(string) }); ok {
		profiled.SetProfile(cfg.ClaudeProfile)
	}
	service := runtimeengine.Service{
		Home: a.home, Store: s, Adapter: adapter, ExternalReceiver: externalReceiver,
		Analyzer: bundle.Analyzer, Executor: bundle.Executor, Actioner: bundle.Actioner, Reviewer: bundle.Reviewer,
		Logger: logger, Diagnostic: a.errOut,
	}
	if externalReceiver && a.runtimeWake != nil {
		var unsubscribe func()
		service.Wake, unsubscribe = a.runtimeWake.Subscribe(cfg.ChannelID)
		defer unsubscribe()
	}
	// A group_mention runtime discovers its groups from the DWS context
	// channel, never from the application Stream adapter above, which
	// has no history or group listing capability of its own.
	if cfg.ApplicationMode == "group_mention" && cfg.ContextChannelID != "" {
		groupSource, sourceErr := a.runtimeAdapter(ctx, s, cfg.ContextChannelID)
		if sourceErr != nil {
			return sourceErr
		}
		if lister, ok := groupSource.(channel.GroupLister); ok {
			service.GroupSource = lister
		}
	}
	if logErr != nil {
		service.Logger = nil
		if a.errOut != nil {
			_, _ = a.errOut.Write([]byte("runtime logging degraded\n"))
		}
	}
	stopObservation, observationErr := observation.Watch(ctx, a.home, cfg.ID, Version, observation.ExecutableBuild())
	if observationErr == nil {
		defer stopObservation()
	} else if a.errOut != nil {
		_, _ = a.errOut.Write([]byte("runtime heartbeat observation unavailable\n"))
	}
	return service.Run(ctx, cfg.ID)
}

type runtimeSetupDiscoverer interface {
	DiscoverRuntimeSetup(context.Context, dws.RuntimeSetupRequest) (dws.RuntimeSetupResult, error)
}

// runtime setup is the opinionated first-run path. It resolves dws identity and
// conversation IDs, creates the preset/workspace/channel/routes, verifies the
// actual platform capabilities, and persists the runtime in one command.
func (a *app) runtimeSetupCommand() *cobra.Command {
	var profile, robotCode, robotName, deliveryConversation, workspacePath, workspaceName, channelName, preset, agentHarness string
	var claudeProfile, analysisModel, executionModel string
	var ignored []string
	var pilot bool
	var threshold, maxWait, reconcile, concurrency int
	var scheduling core.Scheduling
	cmd := &cobra.Command{
		Use:   "setup <name>",
		Short: "自动发现并配置一个钉钉值守实例",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), a.timeout)
			defer cancel()
			if err := a.cfg.RuntimeSetup.apply(cmd); err != nil {
				return err
			}
			for name, value := range map[string]int{"analysis-concurrency": scheduling.AnalysisConcurrency, "execution-concurrency": scheduling.ExecutionConcurrency, "analysis-timeout-seconds": scheduling.AnalysisTimeoutSeconds, "execution-timeout-seconds": scheduling.ExecutionTimeoutSeconds, "review-timeout-seconds": scheduling.ReviewTimeoutSeconds} {
				if cmd.Flags().Changed(name) && value <= 0 {
					return core.Fail("invalid_input", "%s must be positive", name)
				}
			}
			if threshold < 0 || threshold > 100 || maxWait < 0 || maxWait > 86400 || reconcile < 0 || reconcile > 86400 || (reconcile > 0 && reconcile < 10) || concurrency < 0 || concurrency > 32 {
				return core.Fail("invalid_input", "invalid runtime setup thresholds or concurrency")
			}
			name := args[0]
			agentHarness = strings.ToLower(strings.TrimSpace(agentHarness))
			if agentHarness == "" {
				agentHarness = "claude"
			}
			if agentHarness != "claude" && claudeProfile != "" {
				return core.Fail("invalid_input", "claude-profile is supported only by the claude harness")
			}
			if agentHarness != "claude" && analysisModel == "" {
				return core.Fail("invalid_input", "analysis-model is required for a non-Claude harness")
			}
			if analysisModel == "" {
				analysisModel = "haiku"
			}
			if claudeProfile != "" {
				if executionModel == "" {
					executionModel = "profile"
				}
			}
			if _, err := runtimeengine.NewHarness(agentHarness, analysisModel, executionModel); err != nil {
				return core.Fail("invalid_input", "the selected agent harness is unavailable or incomplete; inspect runtime harness")
			}
			if preset == "" {
				preset = agentHarness + "-default"
			}
			adapter := a.dwsAdapter
			if adapter == nil {
				adapter = dws.New()
			}
			discoverer, ok := adapter.(runtimeSetupDiscoverer)
			if !ok {
				return core.Fail("unavailable", "the selected dws adapter cannot discover runtime setup")
			}
			discovered, err := discoverer.DiscoverRuntimeSetup(ctx, dws.RuntimeSetupRequest{Profile: profile, Ignore: ignored, RobotCode: robotCode, RobotName: robotName, DeliveryConversationID: deliveryConversation})
			if err != nil {
				return err
			}
			presetState, err := agent.Enable(ctx, a.home, agentHarness, preset)
			if err != nil {
				return err
			}
			if workspacePath == "" {
				workspacePath, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			workspacePath, err = filepath.Abs(workspacePath)
			if err != nil {
				return err
			}
			workspaceInfo, err := os.Stat(workspacePath)
			if err != nil {
				return core.Fail("invalid_input", "workspace path is not accessible: %v", err)
			}
			if !workspaceInfo.IsDir() {
				return core.Fail("invalid_input", "workspace path must be a directory")
			}
			if workspaceName == "" {
				workspaceName = name
			}
			if channelName == "" {
				channelName = name + "-dingtalk"
			}

			identity := core.ChannelIdentity{Profile: discovered.Profile, ExpectedCorpID: discovered.CorpID,
				ExpectedUserID: discovered.OwnerUserID, DeliveryRobotCode: discovered.RobotCode, DeliveryRobotName: discovered.RobotName}
			conversations := []string{}
			for _, conversation := range discovered.Conversations {
				if !conversation.Ignored {
					conversations = append(conversations, conversation.ID)
				}
			}
			if len(conversations) == 0 {
				return core.Fail("invalid_input", "every selected group is ignored; at least one group must remain watched")
			}
			probeConfig := channel.Config{ChannelName: channelName, Kind: core.ChannelDwsPersonal, Tenant: discovered.CorpID,
				Identity: identity, Conversations: conversations}
			caps, err := adapter.ProbeCapabilities(ctx, probeConfig)
			if err != nil {
				return err
			}
			for _, capability := range []string{"history", "receive"} {
				if !caps.Verified[capability] {
					return core.Fail("unavailable", "dws setup could not verify %s capability", capability)
				}
			}

			s, err := core.Open(ctx, a.dbPath(), true)
			if err != nil {
				return err
			}
			defer s.Close()
			request := core.Request{ID: a.requestID, Command: cmd.CommandPath(), Scope: "global", Actor: a.actor,
				Key: a.key, Input: map[string]any{"name": name, "ignore": ignored, "profile": profile, "workspace_path": workspacePath, "pilot": pilot,
					"robot_code": robotCode, "delivery_conversation": deliveryConversation, "workspace_name": workspaceName, "channel_name": channelName,
					"agent_harness": agentHarness, "agent_preset": preset, "claude_profile": claudeProfile, "analysis_model": analysisModel, "execution_model": executionModel,
					"item_threshold": threshold, "max_wait_seconds": maxWait, "reconcile_seconds": reconcile, "concurrency": concurrency}}
			result, err := s.Mutate(ctx, request, func(tx *core.Tx) (any, error) {
				workspace, findErr := findSetupWorkspace(ctx, tx, workspaceName, workspacePath)
				if findErr != nil {
					return nil, findErr
				}
				if workspace.ID == "" {
					workspace, findErr = tx.AddWorkspace(ctx, workspaceName, workspacePath)
					if findErr != nil {
						return nil, findErr
					}
				}
				stored, addErr := tx.AddChannel(ctx, core.ChannelInput{Name: channelName, Kind: core.ChannelDwsPersonal, Identity: identity})
				if addErr != nil {
					return nil, addErr
				}
				watchedRouteIDs := []string{}
				for _, conversation := range discovered.Conversations {
					mode := "collect"
					if conversation.Ignored {
						mode = "ignore"
					}
					route, routeErr := tx.AddRoute(ctx, stored.ID, core.RouteInput{ConversationID: conversation.ID,
						ConversationType: "group", Workspace: workspace.ID, Mode: mode})
					if routeErr != nil {
						return nil, routeErr
					}
					if !conversation.Ignored {
						watchedRouteIDs = append(watchedRouteIDs, route.ID)
					}
				}
				stored, addErr = tx.SetChannelCapabilities(ctx, stored.ID, caps, adapter.Name())
				if addErr != nil {
					return nil, addErr
				}
				if _, addErr = tx.AttestDWSOwner(ctx, stored.ID, stored.ConfigVersion); addErr != nil {
					return nil, addErr
				}
				config := core.RuntimeConfigInput{Scheduling: scheduling, Name: name, Channel: stored.ID, RouteIDs: watchedRouteIDs,
					Owner:         core.Sender{IDType: "user_id", IDValue: discovered.OwnerUserID, DisplayName: discovered.OwnerName},
					ClaudeProfile: claudeProfile, AnalysisModel: analysisModel, ExecutionModel: executionModel, AgentPreset: preset, ItemThreshold: threshold, MaxWaitSeconds: maxWait, ReconcileSeconds: reconcile, Concurrency: concurrency}
				if pilot {
					config.ItemThreshold, config.MaxWaitSeconds, config.ReconcileSeconds = 1, 30, 10
				}
				runtimeConfig, configErr := tx.ConfigureRuntime(ctx, config)
				if configErr != nil {
					return nil, configErr
				}
				stored, configErr = core.ReadChannel(ctx, tx.Conn, stored.ID)
				if configErr != nil {
					return nil, configErr
				}
				return map[string]any{"runtime": runtimeConfig, "channel": stored, "workspace": workspace, "preset": presetState,
					"discovered":   map[string]any{"profile": discovered.Profile, "owner_name": discovered.OwnerName, "group_count": len(discovered.Conversations), "watched_group_count": len(watchedRouteIDs), "group_discovery_complete": discovered.GroupDiscoveryComplete, "ignored_count": len(discovered.Conversations) - len(watchedRouteIDs)},
					"next_command": "memgov runtime start " + name}, nil
			})
			if err != nil {
				return err
			}
			return a.emit(result.Data, result.Cached)
		},
	}
	cmd.Flags().StringArrayVar(&ignored, "ignore", nil, "忽略的群名称或 ID；可重复传入")
	cmd.Flags().StringVar(&profile, "profile", "", "dws profile；默认使用当前 profile")
	cmd.Flags().StringVar(&robotCode, "robot-code", "", "可选的群范围筛选机器人 Code")
	cmd.Flags().StringVar(&robotName, "robot-name", "", "企业应用机器人名称；用于自动筛选机器人所在群")
	cmd.Flags().StringVar(&deliveryConversation, "delivery-conversation", "", "旧版完成通知参数；后台任务现在仅记录结果")
	_ = cmd.Flags().MarkDeprecated("delivery-conversation", "Cyber owner 完成策略为 record_only；主动沟通由 Agent 明确发起")
	cmd.Flags().StringVar(&workspacePath, "workspace-path", "", "任务项目目录；默认当前目录")
	cmd.Flags().StringVar(&workspaceName, "workspace-name", "", "memgov 工作区名称；默认与 runtime 同名")
	cmd.Flags().StringVar(&channelName, "channel-name", "", "通道名称；默认 <runtime>-dingtalk")
	cmd.Flags().StringVar(&preset, "agent-preset", "", "受控 Agent preset；默认 <agent-harness>-default")
	cmd.Flags().StringVar(&agentHarness, "agent-harness", "claude", "Agent harness 注册名；可通过 runtime harness 查看")
	cmd.Flags().StringVar(&claudeProfile, "claude-profile", "", "从本机 zsh alias 读取 Claude 环境配置和默认模型")
	cmd.Flags().StringVar(&analysisModel, "analysis-model", "", "增量分析模型；profile 表示使用 Claude profile 默认模型")
	cmd.Flags().StringVar(&executionModel, "execution-model", "", "任务执行模型；profile 表示使用 Claude profile 默认模型")
	cmd.Flags().IntVar(&threshold, "item-threshold", 0, "新增消息触发条数，默认 20，范围 1—100")
	cmd.Flags().IntVar(&maxWait, "max-wait-seconds", 0, "最长等待秒数，默认 30，范围 1—86400")
	cmd.Flags().IntVar(&reconcile, "reconcile-seconds", 0, "补漏间隔秒数，默认 300，范围 10—86400")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "执行并发兼容字段，范围 1—32")
	cmd.Flags().IntVar(&scheduling.AnalysisConcurrency, "analysis-concurrency", 0, "分析并发，默认 8，范围 1—32")
	cmd.Flags().IntVar(&scheduling.ExecutionConcurrency, "execution-concurrency", 0, "执行并发，旧配置默认 1，范围 1—32")
	cmd.Flags().IntVar(&scheduling.AnalysisTimeoutSeconds, "analysis-timeout-seconds", 0, "分析截止秒数，默认 120")
	cmd.Flags().IntVar(&scheduling.ExecutionTimeoutSeconds, "execution-timeout-seconds", 0, "任务总截止秒数，默认 900")
	cmd.Flags().IntVar(&scheduling.ReviewTimeoutSeconds, "review-timeout-seconds", 0, "审查截止秒数，默认 120")
	cmd.Flags().BoolVar(&pilot, "pilot", false, "使用 1 条或 30 秒触发的验证参数")
	return cmd
}

func findSetupWorkspace(ctx context.Context, tx *core.Tx, name, path string) (core.Workspace, error) {
	var workspace core.Workspace
	err := tx.Conn.QueryRowContext(ctx, "SELECT id,name,coalesce(path,'') FROM workspaces WHERE name=? OR path=?", name, path).
		Scan(&workspace.ID, &workspace.Name, &workspace.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Workspace{}, nil
	}
	if err != nil {
		return workspace, err
	}
	if workspace.Path != path {
		return workspace, core.Fail("conflict", "workspace %q already points to a different path", workspace.Name)
	}
	return workspace, nil
}

func (a *app) runtimeAdapter(ctx context.Context, s *core.Store, channelID string) (channel.Adapter, error) {
	c, err := core.ReadChannel(ctx, s.DB, channelID)
	if err != nil {
		return nil, err
	}
	return a.adapterFor(c)
}

func runtimeConfig(ctx context.Context, a *app, value string) (core.RuntimeConfig, error) {
	s, err := core.Open(ctx, a.dbPath(), false)
	if err != nil {
		return core.RuntimeConfig{}, err
	}
	defer s.Close()
	return core.ReadRuntime(ctx, s.DB, value)
}

func addLogFlags(cmd *cobra.Command, since, until, level, component, batchID, taskID *string, limit *int) {
	cmd.Flags().StringVar(since, "since", "", "起始时间 RFC3339")
	cmd.Flags().StringVar(until, "until", "", "结束时间 RFC3339")
	cmd.Flags().StringVar(level, "level", "", "debug、info、warn、error")
	cmd.Flags().StringVar(component, "component", "", "组件过滤")
	cmd.Flags().StringVar(batchID, "batch", "", "批次 ID")
	cmd.Flags().StringVar(taskID, "task", "", "任务 ID")
	cmd.Flags().IntVar(limit, "limit", 200, "最多返回最近条目")
}
