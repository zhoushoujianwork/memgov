package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/processtree"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

type Service struct {
	Home    string
	Store   *core.Store
	Adapter channel.Adapter
	// GroupSource is an optional read-only group lister used by a
	// group_mention runtime to discover DWS groups active in the last 30
	// days. The application Stream adapter above remains the only receive
	// and send path; GroupSource never receives or sends anything.
	ExternalReceiver bool // Unified service owns the channel receiver.
	GroupSource      channel.GroupLister
	Analyzer         Analyzer
	Executor         Executor
	Actioner         ActionExecutor
	Reviewer         Reviewer
	// HarnessName binds every selected task preset to the adapter instantiated
	// at startup. Empty is reserved for hosts injecting their own components.
	HarnessName string
	Logger      *runlog.Logger
	Diagnostic  io.Writer
	Tick        time.Duration
	Now         func() time.Time
	// Wake is signaled after the shared receiver durably commits an event.
	// The one-second ticker remains a recovery fallback.
	Wake <-chan struct{}
	// Notify publishes committed intake when this Service owns a receiver.
	Notify func(string)

	mu              sync.Mutex
	logFailed       bool
	reconcileCursor map[string]int
	stageQueue      chan stageDelivery
	workers         sync.WaitGroup
	concurrent      bool
	workerWake      chan struct{}
	slotsMu         sync.Mutex
	analysisSlots   int
	executionSlots  int
}

type stageDelivery struct {
	Config  core.RuntimeConfig
	TaskID  string
	Purpose string
}

func (s *Service) reserveSlot(cfg core.RuntimeConfig, kind string) bool {
	if cfg.ApplicationMode != "proactive" || !s.concurrent {
		return true
	}
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()
	if kind == "analysis" {
		if s.analysisSlots >= cfg.AnalysisConcurrency {
			return false
		}
		s.analysisSlots++
	} else {
		if s.executionSlots >= cfg.Concurrency {
			return false
		}
		s.executionSlots++
	}
	return true
}

func (s *Service) releaseSlot(cfg core.RuntimeConfig, kind string) {
	if cfg.ApplicationMode != "proactive" || !s.concurrent {
		return
	}
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()
	if kind == "analysis" && s.analysisSlots > 0 {
		s.analysisSlots--
	}
	if kind != "analysis" && s.executionSlots > 0 {
		s.executionSlots--
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func (s *Service) mutate(ctx context.Context, scope, command string, fn func(*core.Tx) (any, error)) error {
	if command == "runtime.task.complete" || command == "runtime.batch.complete" || command == "runtime.action.complete" || command == "runtime.memory.review" {
		if err := processtree.CheckQuiescence(ctx); err != nil {
			return core.Fail("process_cleanup_failed", "%s", err)
		}
	}
	_, err := s.Store.Mutate(ctx, core.Request{ID: core.NewID(), Scope: scope, Command: command, Actor: "runtime"}, fn)
	return err
}
func (s *Service) emit(ctx context.Context, e runlog.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Logger == nil || s.logFailed {
		return
	}
	if err := s.Logger.Emit(e); err != nil {
		s.logFailed = true
		if s.Diagnostic != nil {
			fmt.Fprintln(s.Diagnostic, "runtime logging degraded")
		}
		_ = s.mutate(ctx, "global", "runtime.degraded", func(tx *core.Tx) (any, error) {
			return tx.SetRuntimeStatus(ctx, e.RuntimeID, "degraded", "runtime_log_unavailable")
		})
	}
}
func (s *Service) loggerHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Logger != nil && !s.logFailed
}

func (s *Service) Run(ctx context.Context, value string) error {
	ctx, cancelRun := context.WithCancel(ctx)
	s.concurrent = true
	s.workerWake = make(chan struct{}, 1)
	defer func() { cancelRun(); s.workers.Wait(); s.concurrent = false }()
	if closer, ok := s.Executor.(interface{ CloseDirectSessions() }); ok {
		defer closer.CloseDirectSessions()
	}
	config, err := core.ReadRuntime(ctx, s.Store.DB, value)
	if err != nil {
		return err
	}
	preset, err := agent.Status(ctx, s.Home, config.AgentPreset)
	if err != nil {
		return err
	}
	if preset.Status != "enabled" || !preset.Clean {
		return core.Fail("denied", "agent preset must be enabled and clean before runtime start")
	}
	if err = s.checkPresetHarness(preset); err != nil {
		return err
	}
	channelValue, err := core.ReadChannel(ctx, s.Store.DB, config.ChannelID)
	if err != nil {
		return err
	}
	if !runtimeCapabilitiesReady(channelValue, config) {
		return core.Fail("unavailable", "runtime channel requires verified receive and history for observation, or receive and send for bot interaction")
	}
	if s.Analyzer == nil || s.Executor == nil || s.Reviewer == nil || s.Adapter == nil {
		return core.Fail("unavailable", "runtime analyzer, executor, reviewer and channel adapter are required")
	}
	if s.Actioner == nil {
		if actioner, ok := s.Executor.(ActionExecutor); ok {
			s.Actioner = actioner
		} else {
			return core.Fail("unavailable", "runtime confirmed-action executor is required")
		}
	}
	var recovery core.RuntimeRecovery
	if err = s.recoverWork(ctx, config.ID); err != nil {
		return err
	}
	if err = s.mutate(ctx, "global", "runtime.recover", func(tx *core.Tx) (any, error) {
		var recoverErr error
		recovery, recoverErr = tx.RecoverRuntime(ctx, config.ID)
		return recovery, recoverErr
	}); err != nil {
		return err
	}
	err = s.mutate(ctx, "global", "runtime.start", func(tx *core.Tx) (any, error) { return tx.SetRuntimeStatus(ctx, config.ID, "running", "") })
	if err != nil {
		return err
	}
	config, _ = core.ReadRuntime(ctx, s.Store.DB, config.ID)
	runtimeID := config.ID
	s.emit(ctx, runlog.Event{RuntimeID: config.ID, Level: "info", Component: "runtime", Event: "started", Status: "running", Summary: "AI 值守已启动"})
	if recovery.Batches+recovery.Tasks+recovery.ActionsUnknown+recovery.DeliveriesUnknown > 0 {
		s.emit(ctx, runlog.Event{RuntimeID: config.ID, Level: "warn", Component: "runtime", Event: "recovered", Status: "recovered", Summary: fmt.Sprintf("恢复 %d 个批次、%d 个任务、%d 个未知外部操作和 %d 个未知投递", recovery.Batches, recovery.Tasks, recovery.ActionsUnknown, recovery.DeliveriesUnknown)})
	}
	s.reconcileStageAcknowledgements(ctx, config)
	s.reconcileFailureNotices(ctx, config)
	stageCtx, cancelStages := context.WithCancel(ctx)
	s.stageQueue = make(chan stageDelivery, 32)
	stageDone := make(chan struct{})
	go func() {
		defer close(stageDone)
		s.runStageDeliveries(stageCtx)
	}()
	defer func() {
		cancelRun()
		s.workers.Wait()
		cancelStages()
		<-stageDone
		s.stageQueue = nil
	}()
	if !s.loggerHealthy() {
		_ = s.mutate(ctx, "global", "runtime.degraded", func(tx *core.Tx) (any, error) {
			return tx.SetRuntimeStatus(ctx, config.ID, "degraded", "runtime_log_unavailable")
		})
	}
	_, independentSource, err := core.DataSourceForRuntime(ctx, s.Store.DB, config.ID)
	if err != nil {
		return err
	}
	receiverCtx, cancelReceiver := context.WithCancel(ctx)
	receiverDone := make(chan struct{})
	receiverConfig := config
	defer func() {
		cancelReceiver()
		select {
		case <-receiverDone:
		case <-time.After(6 * time.Second):
		}
	}()
	go func() {
		defer close(receiverDone)
		if !independentSource && !s.ExternalReceiver {
			s.receiveLoop(receiverCtx, receiverConfig, channelValue)
		}
	}()
	// Route discovery and bounded history repair can call DWS for up to two
	// minutes. Keep them off the interactive intake loop; workers continue using
	// the last verified route set until the refresh commits a replacement.
	reconcileCtx, cancelReconcile := context.WithCancel(ctx)
	reconcileRequests := make(chan struct{}, 1)
	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		for {
			select {
			case <-reconcileCtx.Done():
				return
			case <-reconcileRequests:
				current, readErr := core.ReadRuntime(reconcileCtx, s.Store.DB, runtimeID)
				if readErr != nil {
					continue
				}
				current = s.syncGroups(reconcileCtx, current, channelValue)
				s.reconcile(reconcileCtx, current, channelValue)
				s.maintainLogger(reconcileCtx, current.ID)
			}
		}
	}()
	defer func() {
		cancelReconcile()
		<-reconcileDone
	}()
	requestReconcile := func() {
		select {
		case reconcileRequests <- struct{}{}:
		default:
		}
	}
	requestReconcile()
	s.tick(ctx, config, preset)
	interval := s.Tick
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	reconcileTicker := time.NewTicker(time.Duration(config.ReconcileSeconds) * time.Second)
	defer reconcileTicker.Stop()
	process := func() (bool, error) {
		current, readErr := core.ReadRuntime(ctx, s.Store.DB, runtimeID)
		if readErr != nil {
			if ctx.Err() != nil {
				_ = s.stop(context.Background(), runtimeID, "context_cancelled")
				return true, nil
			}
			return false, readErr
		}
		if current.Status == "stopped" {
			cancelReceiver()
			s.emit(ctx, runlog.Event{RuntimeID: runtimeID, Level: "info", Component: "runtime", Event: "stopped", Status: "stopped", Summary: "AI 值守已停止"})
			return true, nil
		}
		if current.Status == "paused" || current.Status == "degraded" {
			return false, nil
		}
		config = current
		s.tick(ctx, config, preset)
		return false, nil
	}
	for {
		select {
		case <-ctx.Done():
			_ = s.stop(context.Background(), runtimeID, "context_cancelled")
			return nil
		case <-reconcileTicker.C:
			requestReconcile()
		case <-ticker.C:
			stopped, processErr := process()
			if processErr != nil {
				return processErr
			}
			if stopped {
				return nil
			}
		case <-s.Wake:
			stopped, processErr := process()
			if processErr != nil {
				return processErr
			}
			if stopped {
				return nil
			}
		case <-s.workerWake:
			stopped, processErr := process()
			if processErr != nil {
				return processErr
			}
			if stopped {
				return nil
			}
		}
	}
}

func (s *Service) maintainLogger(ctx context.Context, runtimeID string) {
	s.mu.Lock()
	if s.Logger == nil || s.logFailed {
		s.mu.Unlock()
		return
	}
	err := s.Logger.Maintain()
	if err != nil {
		s.logFailed = true
	}
	s.mu.Unlock()
	if err == nil {
		return
	}
	if s.Diagnostic != nil {
		fmt.Fprintln(s.Diagnostic, "runtime logging degraded")
	}
	_ = s.mutate(ctx, "global", "runtime.degraded", func(tx *core.Tx) (any, error) {
		return tx.SetRuntimeStatus(ctx, runtimeID, "degraded", "runtime_log_unavailable")
	})
}

// Application bots receive new messages from DingTalk Stream but do not have a
// history API. Their runtime starts at bootstrap_at and relies on the stream's
// durable acknowledgement. Personal DWS watchers still require history because
// they use it for bounded reconciliation and gap repair.
func runtimeCapabilitiesReady(c core.Channel, cfg core.RuntimeConfig) bool {
	if !c.Capabilities.Verified["receive"] {
		return false
	}
	if cfg.ApplicationMode != "proactive" && !c.Capabilities.Verified["send"] {
		return false
	}
	return c.Kind == core.ChannelDingTalkApp || c.Capabilities.Verified["history"]
}

func (s *Service) stop(ctx context.Context, id, reason string) error {
	return s.mutate(ctx, "global", "runtime.stop", func(tx *core.Tx) (any, error) { return tx.SetRuntimeStatus(ctx, id, "stopped", reason) })
}

func (s *Service) receiveLoop(ctx context.Context, cfg core.RuntimeConfig, c core.Channel) {
	shared := c.Kind == core.ChannelDingTalkApp
	actor := "runtime:" + cfg.ID
	ttl := 15 * time.Minute
	if shared {
		// A process-specific holder prevents a restarted/duplicate runtime from
		// stealing a healthy receiver. The short renewed lease bounds crash failover.
		actor += ":" + core.NewID()
		ttl = 30 * time.Second
	}
	standby := false
	waitForReceiver := func() bool {
		if !standby {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "info", Component: "dingtalk", Event: "receiver_standby", Status: "standby", Summary: "应用通道已有接收者，本实例继续处理自己的路由并等待接管"})
			standby = true
		}
		return waitReceiver(ctx, 500*time.Millisecond)
	}
	for ctx.Err() == nil {
		if shared {
			lease, err := core.ReadLease(ctx, s.Store.DB, c.ID)
			if err == nil && lease.Held && lease.Holder != actor+"@receiver" {
				if !waitForReceiver() {
					return
				}
				continue
			}
			if err != nil {
				s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "dingtalk", Event: "receiver_lease_failed", ErrorCode: core.ErrorCode(err), Summary: "应用通道接收租约暂时不可读取"})
				if !waitReceiver(ctx, time.Second) {
					return
				}
				continue
			}
		}
		// No runtime-specific subset: one application Stream ingests all channel
		// routes; each runtime's independent tick selects only its own routes.
		collector := channel.Collector{Store: s.Store, Adapter: s.Adapter, OnIntake: func(result core.IntakeResult) {
			if s.Notify != nil {
				s.Notify(result.ChannelID)
			}
		}}
		result, err := collector.Receive(ctx, core.Request{ID: core.NewID(), Scope: "global", Actor: actor}, c.ID, ttl)
		if ctx.Err() != nil {
			return
		}
		if result.Lease.Token != "" {
			standby = false
		}
		if shared && core.ErrorCode(err) == "conflict" {
			// The read above is only an optimization. AcquireLease is the atomic
			// arbiter when two candidates observe an available channel together.
			if !waitForReceiver() {
				return
			}
			continue
		}
		if err != nil {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "dingtalk", Event: "receiver_reconnect", ErrorCode: core.ErrorCode(err), Summary: "消息长连接已中断，准备重连"})
		}
		if !waitReceiver(ctx, time.Second) {
			return
		}
	}
}

func waitReceiver(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// syncGroups refreshes active processing routes without disturbing the receiver.
// Independent sources may retain unobserved routes for collection continuity;
// only positive discovery evidence authorizes their AI consumer's scope.
func (s *Service) sourceProcessingRoutes(ctx context.Context, q core.Queryer, cfg core.RuntimeConfig) (groups []string, bound bool, proofErr, err error) {
	source, bound, err := core.DataSourceForRuntime(ctx, q, cfg.ID)
	if err != nil || !bound {
		return nil, bound, nil, err
	}
	if cfg.ApplicationMode == "direct" || cfg.ApplicationMode == "proactive" {
		groups, proofErr = core.ReadOwnerSourceProcessingRoutes(ctx, q, source, s.now())
	} else {
		groups, proofErr = core.ReadSourcePositiveProcessingRoutes(ctx, q, source, s.now())
	}
	if proofErr != nil || groups == nil {
		groups = []string{}
	}
	return groups, true, proofErr, nil
}

func (s *Service) syncGroups(ctx context.Context, cfg core.RuntimeConfig, c core.Channel) core.RuntimeConfig {
	readTx, err := s.Store.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		cfg.RouteIDs = []string{}
		return cfg
	}
	sourceGroups, bound, proofErr, readErr := s.sourceProcessingRoutes(ctx, readTx, cfg)
	if readErr == nil {
		readErr = readTx.Commit()
	} else {
		_ = readTx.Rollback()
	}
	if readErr != nil {
		cfg.RouteIDs = []string{}
		return cfg
	}
	if bound {
		// Most ticks only revalidate unchanged authorization. Keep that read in a
		// WAL snapshot so bot receipts and lease renewals do not wait behind it.
		// A changed scope is recomputed below inside BEGIN IMMEDIATE before it is
		// persisted, preserving the authorization boundary.
		if slices.Equal(sourceGroups, cfg.RouteIDs) {
			if proofErr != nil {
				s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "runtime", Event: "group_sync_failed", ErrorCode: core.ErrorCode(proofErr), Summary: "来源群证据不可用，已清空 AI 处理范围"})
			}
			return cfg
		}
		var next core.RuntimeConfig
		err = s.mutate(ctx, "global", "runtime.groups.source", func(tx *core.Tx) (any, error) {
			// Read the binding and its proof in the same transaction that replaces
			// processing scope, so a concurrent source reconfiguration cannot
			// authorize routes using an older source snapshot.
			var e error
			sourceGroups, bound, proofErr, e = s.sourceProcessingRoutes(ctx, tx.Conn, cfg)
			if e != nil {
				return nil, e
			}
			if !bound {
				return nil, core.Fail("conflict", "runtime data source binding changed")
			}
			// Absence or revocation of proof must also clear an old broad scope.
			_, e = tx.Conn.ExecContext(ctx, "UPDATE runtime_configs SET route_ids=?,version=version+1,updated_at=? WHERE id=? AND route_ids<>?", mustSourceRoutes(sourceGroups), core.Now(), cfg.ID, mustSourceRoutes(sourceGroups))
			if e != nil {
				return nil, e
			}
			next, e = core.ReadRuntime(ctx, tx.Conn, cfg.ID)
			return nil, e
		})
		if err != nil {
			cfg.RouteIDs = []string{}
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "runtime", Event: "group_sync_failed", ErrorCode: core.ErrorCode(err), Summary: "来源处理范围同步失败，本轮停止群分析"})
			return cfg
		}
		if proofErr != nil {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "runtime", Event: "group_sync_failed", ErrorCode: core.ErrorCode(proofErr), Summary: "来源群证据不可用，已清空 AI 处理范围"})
		}
		return next
	}
	if cfg.ApplicationMode == "group_mention" {
		return s.syncGroupMentionGroups(ctx, cfg)
	}
	lister, ok := s.Adapter.(channel.GroupLister)
	if !ok {
		return cfg
	}
	groups, err := lister.ListGroupConversations(ctx, channel.ConfigFor(c))
	if err != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "dingtalk", Event: "group_sync_failed", ErrorCode: core.ErrorCode(err), Summary: "群列表自动同步失败，继续使用上次范围"})
		return cfg
	}
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	var next core.RuntimeConfig
	var added int
	err = s.mutate(ctx, "global", "runtime.groups.sync", func(tx *core.Tx) (any, error) {
		var syncErr error
		next, added, syncErr = tx.SyncRuntimeGroupRoutes(ctx, cfg.ID, ids)
		return map[string]any{"added": added, "managed": len(ids)}, syncErr
	})
	if err != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "dingtalk", Event: "group_sync_failed", ErrorCode: core.ErrorCode(err), Summary: "新群路由同步失败，继续使用上次范围"})
		return cfg
	}
	if added > 0 || len(next.RouteIDs) != len(cfg.RouteIDs) {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "info", Component: "dingtalk", Event: "groups_synced", Status: "complete", Summary: fmt.Sprintf("值守范围已刷新：%d 个群，新增 %d 个", len(next.RouteIDs), added)})
	}
	return next
}

// syncGroupMentionGroups discovers DWS groups active in the last 30 days
// that still contain the target robot, and reconciles the runtime's
// route_ids against matching assistant routes on the application channel.
// It never touches the Stream receiver: GroupSource is a read-only DWS
// lister, independent from the send/receive adapter above.
func (s *Service) syncGroupMentionGroups(ctx context.Context, cfg core.RuntimeConfig) core.RuntimeConfig {
	if s.GroupSource == nil || cfg.ContextChannelID == "" {
		return cfg
	}
	dwsChannel, err := core.ReadChannel(ctx, s.Store.DB, cfg.ContextChannelID)
	if err != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "dws", Event: "group_sync_failed", ErrorCode: core.ErrorCode(err), Summary: "群助手上下文通道读取失败，继续使用上次范围"})
		return cfg
	}
	observation := channel.GroupDiscovery{Complete: true}
	if detailed, ok := s.GroupSource.(channel.DetailedGroupLister); ok {
		observation, err = detailed.DiscoverGroupConversations(ctx, channel.ConfigFor(dwsChannel))
	} else {
		observation.Groups, err = s.GroupSource.ListGroupConversations(ctx, channel.ConfigFor(dwsChannel))
	}
	if err != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "dws", Event: "group_sync_failed", ErrorCode: core.ErrorCode(err), Summary: "群列表自动同步失败，继续使用上次范围"})
		return cfg
	}
	if !observation.Complete && len(observation.Groups) == 0 {
		return cfg
	}
	ids := make([]string, 0, len(observation.Groups))
	selected := map[string]bool{}
	for _, group := range observation.Groups {
		if !selected[group.ID] {
			ids = append(ids, group.ID)
			selected[group.ID] = true
		}
	}
	if !observation.Complete {
		excluded := map[string]bool{}
		for _, id := range observation.Excluded {
			excluded[id] = true
		}
		for _, routeID := range cfg.RouteIDs {
			route, readErr := core.ReadRoute(ctx, s.Store.DB, routeID)
			if readErr != nil {
				return cfg
			}
			if route.ConversationType == "group" && !excluded[route.ConversationID] && !selected[route.ConversationID] {
				ids = append(ids, route.ConversationID)
				selected[route.ConversationID] = true
			}
		}
	}
	var next core.RuntimeConfig
	var added int
	err = s.mutate(ctx, "global", "runtime.groups.sync", func(tx *core.Tx) (any, error) {
		var syncErr error
		next, added, syncErr = tx.SyncGroupMentionRoutes(ctx, cfg.ID, ids)
		return map[string]any{"added": added, "managed": len(ids)}, syncErr
	})
	if err != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "dws", Event: "group_sync_failed", ErrorCode: core.ErrorCode(err), Summary: "新群路由同步失败，继续使用上次范围"})
		return cfg
	}
	if added > 0 || len(next.RouteIDs) != len(cfg.RouteIDs) {
		status := "complete"
		if !observation.Complete {
			status = "partial"
		}
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "info", Component: "dws", Event: "groups_synced", Status: status, Summary: fmt.Sprintf("群助手挂载已刷新：%d 个群，新增 %d 个", len(next.RouteIDs), added)})
	}
	return next
}

func (s *Service) reconcile(ctx context.Context, cfg core.RuntimeConfig, c core.Channel) {
	if _, bound, err := core.DataSourceForRuntime(ctx, s.Store.DB, cfg.ID); err != nil || bound {
		return
	}
	if !c.Capabilities.Verified["history"] {
		return
	}
	collector := channel.Collector{Store: s.Store, Adapter: s.Adapter}
	const routesPerCycle = 20
	// Delivery is an outbound address and may not be readable as a conversation
	// by the history source. Reconcile only the conversations explicitly watched
	// by this runtime; confirmation messages are handled by the stream sync path.
	routeIDs := cfg.RouteIDs
	if len(routeIDs) == 0 {
		return
	}
	if s.reconcileCursor == nil {
		s.reconcileCursor = map[string]int{}
	}
	startIndex := s.reconcileCursor[cfg.ID] % len(routeIDs)
	limit := min(routesPerCycle, len(routeIDs))
	var events, complete, partial, failed int
	beganCycle := s.now()
	for offset := range limit {
		routeID := routeIDs[(startIndex+offset)%len(routeIDs)]
		route, err := core.ReadRoute(ctx, s.Store.DB, routeID)
		if err != nil || route.Mode == "ignore" {
			continue
		}
		start, end, err := core.NextWindow(ctx, s.Store.DB, c.ID, route.ConversationID, s.now(), time.Hour, 5*time.Minute)
		if err != nil {
			continue
		}
		result, err := collector.Pull(ctx, core.Request{ID: core.NewID(), Scope: route.WorkspaceID, Actor: "runtime:" + cfg.ID}, c.ID, route.ConversationID, start, end, 5, 200)
		if err != nil {
			failed++
			continue
		}
		events += result.Events
		if result.Complete {
			complete++
		} else {
			partial++
		}
	}
	s.reconcileCursor[cfg.ID] = (startIndex + limit) % len(routeIDs)
	level, status := "info", "complete"
	if failed > 0 {
		level, status = "warn", "partial"
	} else if partial > 0 {
		status = "partial"
	}
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: level, Component: "dingtalk", Event: "reconciled", Status: status,
		DurationMS: time.Since(beganCycle).Milliseconds(), Summary: fmt.Sprintf("历史补齐检查 %d 个会话，处理 %d 条消息，失败 %d 个", limit, events, failed)})
}

func (s *Service) tick(ctx context.Context, cfg core.RuntimeConfig, preset agent.Preset) {
	// Revalidate source authorization before every processing pass, including
	// when discovery or configuration changes between reconciliation cycles.
	if _, bound, err := core.DataSourceForRuntime(ctx, s.Store.DB, cfg.ID); err != nil {
		return
	} else if bound {
		cfg = s.syncGroups(ctx, cfg, core.Channel{})
		if len(cfg.RouteIDs) == 0 {
			return
		}
	}
	if !s.loggerHealthy() {
		return
	}
	s.reconcileOwnerMessages(ctx, cfg)
	var confirmed []core.RuntimePendingAction
	confirmationsReady, err := core.RuntimeConfirmationsReady(ctx, s.Store.DB, cfg.ID)
	if err != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "approval", Event: "scan_failed", ErrorCode: core.ErrorCode(err), Summary: "确认消息检查失败"})
	} else if confirmationsReady {
		if err = s.mutate(ctx, "global", "runtime.confirmations", func(tx *core.Tx) (any, error) {
			var confirmErr error
			confirmed, confirmErr = tx.ProcessRuntimeConfirmations(ctx, cfg.ID)
			return confirmed, confirmErr
		}); err != nil {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "approval", Event: "scan_failed", ErrorCode: core.ErrorCode(err), Summary: "确认消息检查失败"})
		} else if len(confirmed) > 0 {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "info", Component: "approval", Event: "confirmed", Status: "confirmed", Summary: fmt.Sprintf("验证通过 %d 个本人私聊确认", len(confirmed))})
		}
	}
	var syncResult core.RuntimeSyncResult
	syncReady, err := core.RuntimeMessagesNeedSync(ctx, s.Store.DB, cfg.ID)
	if err != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "error", Component: "intake", Event: "sync_failed", ErrorCode: core.ErrorCode(err), Summary: "增量消息同步失败"})
		return
	}
	if syncReady {
		err = s.mutate(ctx, "global", "runtime.sync", func(tx *core.Tx) (any, error) {
			var e error
			syncResult, e = tx.SyncRuntimeMessages(ctx, cfg.ID)
			return syncResult, e
		})
		if err != nil {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "error", Component: "intake", Event: "sync_failed", ErrorCode: core.ErrorCode(err), Summary: "增量消息同步失败"})
			return
		}
	}
	if syncResult.Pending > 0 || syncResult.Revised > 0 || syncResult.Recalled > 0 {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "info", Component: "intake", Event: "messages_synced", Status: "ready", Summary: fmt.Sprintf("发现 %d 条待分析消息", syncResult.Pending+syncResult.Revised)})
	}
	for n := 0; ; n++ {
		if cfg.ApplicationMode != "group_mention" && n >= cfg.AnalysisConcurrency {
			break
		}
		if !s.reserveSlot(cfg, "analysis") {
			break
		}
		reservedAnalysis := cfg.ApplicationMode == "proactive" && s.concurrent
		var batch core.RuntimeBatch
		batchReady, err := core.RuntimeBatchReady(ctx, s.Store.DB, cfg.ID, s.now())
		if err != nil {
			if reservedAnalysis {
				s.releaseSlot(cfg, "analysis")
			}
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "error", Component: "analysis", Event: "claim_failed", ErrorCode: core.ErrorCode(err), Summary: "分析批次领取失败"})
			return
		}
		if batchReady {
			err = s.mutate(core.WithInMemoryCapacity(ctx), "global", "runtime.batch.claim", func(tx *core.Tx) (any, error) {
				var e error
				batch, e = tx.ClaimRuntimeBatch(ctx, cfg.ID, s.now())
				return batch, e
			})
			if err != nil {
				if reservedAnalysis {
					s.releaseSlot(cfg, "analysis")
				}
				s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "error", Component: "analysis", Event: "claim_failed", ErrorCode: core.ErrorCode(err), Summary: "分析批次领取失败"})
				return
			}
		}
		if batch.ID != "" {
			if cfg.ApplicationMode == "direct" {
				if reservedAnalysis {
					s.releaseSlot(cfg, "analysis")
				}
				s.queueDirectTurn(ctx, cfg, batch)
			} else if (cfg.ApplicationMode == "proactive" || cfg.ApplicationMode == "group_mention") && s.concurrent {
				s.workers.Add(1)
				go func(batch core.RuntimeBatch) {
					defer s.workers.Done()
					defer s.wakeWorker()
					defer s.releaseSlot(cfg, "analysis")
					s.analyze(ctx, cfg, batch)
				}(batch)
			} else {
				s.analyze(ctx, cfg, batch)
				if reservedAnalysis {
					s.releaseSlot(cfg, "analysis")
				}
			}
		} else if reservedAnalysis {
			s.releaseSlot(cfg, "analysis")
		}
		if batch.ID == "" || (cfg.ApplicationMode != "proactive" && cfg.ApplicationMode != "group_mention") || !s.concurrent {
			break
		}
	}
	if s.executeOneConfirmedAction(ctx, cfg, preset) {
		return
	}
	for n := 0; ; n++ {
		if cfg.ApplicationMode != "group_mention" && n >= cfg.Concurrency {
			break
		}
		if !s.executeOne(ctx, cfg, preset) {
			break
		}
		if cfg.ApplicationMode != "proactive" && cfg.ApplicationMode != "group_mention" || !s.concurrent {
			break
		}
	}
	if cfg.ApplicationMode == "proactive" {
		for n := 0; n < 2; n++ {
			s.executeReview(ctx, cfg)
		}
	}
}

func (s *Service) executeOneConfirmedAction(ctx context.Context, cfg core.RuntimeConfig, preset agent.Preset) bool {
	if !s.reserveSlot(cfg, "execution") {
		return false
	}
	reserved := cfg.ApplicationMode == "proactive" && s.concurrent
	ready, err := core.RuntimeActionReady(ctx, s.Store.DB, cfg.ID)
	if err != nil || !ready {
		if reserved {
			s.releaseSlot(cfg, "execution")
		}
		return false
	}
	attemptID := core.NewID()
	var action core.RuntimePendingAction
	var attempt core.RuntimeActionAttempt
	err = s.mutate(core.WithInMemoryCapacity(ctx), "global", "runtime.action.claim", func(tx *core.Tx) (any, error) {
		var claimErr error
		action, attempt, claimErr = tx.ClaimRuntimeAction(ctx, cfg.ID, attemptID, cfg.ExecutionModel)
		return map[string]any{"action": action, "attempt": attempt}, claimErr
	})
	if err != nil || action.ID == "" {
		if reserved {
			s.releaseSlot(cfg, "execution")
		}
		return false
	}
	if (cfg.ApplicationMode == "proactive" || cfg.ApplicationMode == "group_mention") && s.concurrent {
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			defer s.wakeWorker()
			defer s.releaseSlot(cfg, "execution")
			s.executeClaimedAction(ctx, cfg, preset, action, attempt)
		}()
	} else {
		s.executeClaimedAction(ctx, cfg, preset, action, attempt)
	}
	return true
}

func (s *Service) executeClaimedAction(ctx context.Context, cfg core.RuntimeConfig, preset agent.Preset, action core.RuntimePendingAction, attempt core.RuntimeActionAttempt) bool {
	if cfg.ApplicationMode == "proactive" {
		var finish func()
		ctx, finish = s.workContext(ctx, attempt.ID, "", 0)
		defer finish()
	}
	task, err := core.ReadRuntimeTask(ctx, s.Store.DB, action.TaskID)
	if err != nil {
		s.failAction(ctx, cfg, action, attempt, err, false)
		return true
	}
	s.processingAcknowledgement(ctx, cfg, task.ID)
	if err = core.RuntimeActionAttemptPolicyCurrent(ctx, s.Store.DB, attempt.ID, action.ID, action.TaskVersion); err != nil {
		s.failAction(ctx, cfg, action, attempt, err, false)
		return true
	}
	policy, selectedPreset, err := s.resolveTaskAgent(ctx, cfg, task)
	if err != nil {
		s.failAction(ctx, cfg, action, attempt, err, false)
		return true
	}
	preset = selectedPreset
	workdir := ""
	for i := len(task.Attempts) - 1; i >= 0; i-- {
		if task.Attempts[i].TaskVersion == task.Version && task.Attempts[i].WorkspaceDir != "" {
			workdir = task.Attempts[i].WorkspaceDir
			break
		}
	}
	if workdir == "" {
		workdir = preset.Path
	}
	if cfg.ApplicationMode == "proactive" {
		roots := []string{workdir}
		roots, err = core.CanonicalWriteRoots(roots)
		if err == nil {
			err = s.mutate(ctx, "global", "runtime.action.lock", func(tx *core.Tx) (any, error) { return nil, tx.LockWorkRoots(ctx, attempt.ID, roots) })
		}
		if err != nil {
			s.failAction(ctx, cfg, action, attempt, err, false)
			return true
		}
		s.phase(ctx, attempt.ID, "confirmed_action")
	}
	start := s.now()
	if err = core.RuntimeActionAttemptPolicyCurrent(ctx, s.Store.DB, attempt.ID, action.ID, action.TaskVersion); err != nil {
		s.failAction(ctx, cfg, action, attempt, err, false)
		return true
	}
	result, executeErr := s.Actioner.ExecuteConfirmedAction(ctx, ActionExecutionInput{Task: task, Action: action, WorkDir: workdir, Preset: preset, PolicyResolved: true, ExecutionModel: policy.ExecutionModel, ClaudeProfile: policy.ClaudeProfile})
	if ctx.Err() != nil {
		executeErr = workError(ctx, "execution")
	}
	if executeErr == nil {
		executeErr = core.RuntimeActionAttemptPolicyCurrent(ctx, s.Store.DB, attempt.ID, action.ID, action.TaskVersion)
	}
	if executeErr != nil {
		s.failAction(ctx, cfg, action, attempt, executeErr, true)
		return true
	}
	var completed core.RuntimeTask
	err = s.mutate(ctx, "global", "runtime.action.complete", func(tx *core.Tx) (any, error) {
		var completeErr error
		completed, completeErr = tx.CompleteRuntimeAction(ctx, action.ID, attempt.ID, action.TaskVersion, result)
		return completed, completeErr
	})
	if err != nil {
		s.failAction(ctx, cfg, action, attempt, err, true)
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: action.TaskID, AttemptID: attempt.ID, Level: "warn", Component: "action", Event: "result_stale", ErrorCode: core.ErrorCode(err), Summary: "外部操作结果与当前任务版本不一致"})
		return true
	}
	inputTokens, outputTokens, cost := usageFields(result.Usage)
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: action.TaskID, AttemptID: attempt.ID, Level: "info", Component: "action", Event: "completed", Status: "executed", Model: attempt.Model, ToolKinds: result.ToolKinds, InputTokens: inputTokens, OutputTokens: outputTokens, CostUSD: cost, DurationMS: s.now().Sub(start).Milliseconds(), Summary: "已执行本人确认的具体外部操作"})
	if completed.Status == "completed" {
		s.deliver(ctx, cfg, completed.ID)
		s.completionAcknowledgement(ctx, cfg, completed.ID)
	}
	return true
}

func (s *Service) failAction(ctx context.Context, cfg core.RuntimeConfig, action core.RuntimePendingAction, attempt core.RuntimeActionAttempt, err error, outcomeUnknown bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	command := "runtime.action.fail"
	if outcomeUnknown {
		command = "runtime.action.unknown"
	}
	failErr := s.mutate(ctx, "global", command, func(tx *core.Tx) (any, error) {
		if outcomeUnknown {
			return tx.UnknownRuntimeAction(ctx, action.ID, attempt.ID, action.TaskVersion, core.ErrorCode(err))
		}
		return tx.FailRuntimeAction(ctx, action.ID, attempt.ID, action.TaskVersion, core.ErrorCode(err))
	})
	status := "failed"
	summary := "确认操作执行前失败"
	if outcomeUnknown {
		status = "unknown"
		summary = "确认操作结果未知，未自动重试"
	}
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: action.TaskID, AttemptID: attempt.ID, Level: "error", Component: "action", Event: status, Status: status, ErrorCode: core.ErrorCode(err), Summary: summary})
	if failErr == nil {
		s.failAcknowledgement(ctx, cfg, action.TaskID)
	}
}

func (s *Service) analyze(ctx context.Context, cfg core.RuntimeConfig, batch core.RuntimeBatch) {
	if cfg.ApplicationMode == "proactive" {
		var finish func()
		ctx, finish = s.workContext(ctx, batch.ID, "", 0)
		defer finish()
	}
	start := s.now()
	component, completeSummary, invalidSummary := "analysis", "识别到 %d 个待处理事项", "Haiku 返回结果未通过验证"
	if cfg.ApplicationMode == "group_mention" {
		component, completeSummary, invalidSummary = "intake", "已接收 %d 条群 @ 消息", "群 @ 接入未通过状态校验"
	}
	var analysis core.RuntimeAnalysis
	var usage ModelUsage
	var err error
	if cfg.ApplicationMode == "group_mention" {
		// Platform-verified mentions already express intent to talk to the Agent.
		// A work classifier must not silently discard greetings or statements.
		for _, message := range batch.Messages {
			// Keep the original topic in recall without exceeding its 30-term
			// query limit. The complete message remains attached to the task.
			words := strings.Fields(message.Body)
			if len(words) > 12 {
				words = words[:12]
			}
			title := []rune(strings.Join(words, " "))
			if len(title) > 120 {
				title = title[:120]
			}
			if len(title) == 0 {
				title = []rune("群内 @ 对话")
			}
			analysis.Decisions = append(analysis.Decisions, core.RuntimeDecision{
				Kind: "task", CanonicalKey: "mention:" + message.ID,
				Title: string(title), MessageIDs: []string{message.ID},
				Instructions: "Answer the requester's original message using authorized same-group context and on-demand memory queries under the current group sharing policy.",
			})
		}
		usage.Model = "local-mention-routing"
	} else {
		s.phase(ctx, batch.ID, "analysis")
		analysis, usage, err = s.Analyzer.Analyze(ctx, batch)
	}
	if ctx.Err() != nil {
		err = workError(ctx, "analysis")
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		ctx = cleanup
		_ = s.mutate(ctx, "global", "runtime.batch.fail", func(tx *core.Tx) (any, error) { return nil, tx.FailRuntimeBatch(ctx, batch.ID, core.ErrorCode(err)) })
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, BatchID: batch.ID, Level: "error", Component: "analysis", Event: "failed", Model: batch.Model, ErrorCode: core.ErrorCode(err), DurationMS: s.now().Sub(start).Milliseconds(), Summary: "Haiku 增量分析失败"})
		return
	}
	// Proactive consumers use semantic intake; group mentions are routed directly.
	var tasks []core.RuntimeTask
	err = s.mutate(ctx, "global", "runtime.batch.complete", func(tx *core.Tx) (any, error) {
		var e error
		tasks, e = tx.CompleteRuntimeBatch(ctx, batch, analysis)
		return tasks, e
	})
	if err != nil {
		_ = s.mutate(ctx, "global", "runtime.batch.fail", func(tx *core.Tx) (any, error) { return nil, tx.FailRuntimeBatch(ctx, batch.ID, core.ErrorCode(err)) })
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, BatchID: batch.ID, Level: "error", Component: component, Event: "invalid_result", Model: batch.Model, ErrorCode: core.ErrorCode(err), Summary: invalidSummary})
		return
	}
	analysisModel := strings.TrimSpace(usage.Model)
	if analysisModel == "" {
		analysisModel = batch.Model
	}
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, BatchID: batch.ID, Level: "info", Component: component, Event: "completed", Status: "completed", Model: analysisModel, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CostUSD: usage.CostUSD, DurationMS: s.now().Sub(start).Milliseconds(), Summary: fmt.Sprintf(completeSummary, len(tasks))})
	if cfg.ApplicationMode == "group_mention" {
		for _, task := range tasks {
			s.acknowledge(ctx, cfg, task.ID)
		}
	}
	for _, task := range tasks {
		if task.Status == "clarification" || task.Status == "completed" {
			s.deliver(ctx, cfg, task.ID)
			if task.Status == "completed" {
				s.completionAcknowledgement(ctx, cfg, task.ID)
			}
		}
	}
}

func (s *Service) executeOne(ctx context.Context, cfg core.RuntimeConfig, preset agent.Preset) bool {
	if !s.reserveSlot(cfg, "execution") {
		return false
	}
	reserved := cfg.ApplicationMode == "proactive" && s.concurrent
	ready, err := core.RuntimeTaskReady(ctx, s.Store.DB, cfg.ID)
	if err != nil || !ready {
		if reserved {
			s.releaseSlot(cfg, "execution")
		}
		return false
	}
	attemptID := core.NewID()
	var task core.RuntimeTask
	var attempt core.RuntimeAttempt
	err = s.mutate(core.WithInMemoryCapacity(ctx), "global", "runtime.task.claim", func(tx *core.Tx) (any, error) {
		var e error
		task, attempt, e = tx.ClaimRuntimeTask(ctx, cfg.ID, attemptID, cfg.ExecutionModel, preset.Name, preset.Commit, "")
		return map[string]any{"task": task, "attempt": attempt}, e
	})
	if err != nil || task.ID == "" {
		if reserved {
			s.releaseSlot(cfg, "execution")
		}
		return false
	}
	if (cfg.ApplicationMode == "proactive" || cfg.ApplicationMode == "group_mention") && s.concurrent {
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			defer s.wakeWorker()
			defer s.releaseSlot(cfg, "execution")
			s.executeClaimed(ctx, cfg, preset, task, attempt)
		}()
		return true
	}
	s.executeClaimed(ctx, cfg, preset, task, attempt)
	return true
}

func (s *Service) executeClaimed(ctx context.Context, cfg core.RuntimeConfig, preset agent.Preset, task core.RuntimeTask, attempt core.RuntimeAttempt) {
	var finishTask func()
	ctx, finishTask = s.taskContext(ctx, task.ID, task.Version)
	defer finishTask()
	if cfg.ApplicationMode == "proactive" || cfg.ApplicationMode == "group_mention" {
		var finish func()
		ctx, finish = s.workContext(ctx, attempt.ID, task.ID, task.Version)
		defer finish()
	}
	// A committed turn may be recovered after a crash before its receipt queue
	// entry was consumed. Re-enqueueing is safe because the outbox is idempotent;
	// in a live Service this is asynchronous and does not gate Agent execution.
	if cfg.ApplicationMode == "direct" || cfg.ApplicationMode == "group_mention" {
		s.acknowledge(ctx, cfg, task.ID)
	}
	s.processingAcknowledgement(ctx, cfg, task.ID)
	if cfg.ApplicationMode == "direct" {
		s.executeDirectTurn(ctx, cfg, task, attempt)
		return
	}
	policy, selectedPreset, err := s.resolveTaskAgent(ctx, cfg, task)
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	preset = selectedPreset
	err = s.mutate(ctx, "global", "runtime.attempt.agent", func(tx *core.Tx) (any, error) {
		return nil, tx.RecordRuntimeAttemptAgent(ctx, cfg, task, attempt.ID, policy, preset.Commit)
	})
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	attempt.Model, attempt.PresetName, attempt.PresetCommit = policy.ExecutionModel, preset.Name, preset.Commit
	workspace, err := core.RuntimeTaskWorkspace(ctx, s.Store.DB, task)
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	workspaceForAttempt := workspace
	if cfg.ApplicationMode == "group_mention" {
		// Group answers are content work by default. Keep them in an isolated task
		// directory and do not expose a repository merely because the DWS route is
		// associated with one.
		workspaceForAttempt = core.Workspace{}
	}
	var declaredWorkspace *OwnerDirectoryWorkspace
	var workdir, branch, base string
	if task.Resume != nil {
		var previous core.RuntimeAttempt
		previous, branch, base, err = resumeWorkspace(ctx, s.Home, task, workspaceForAttempt)
		workdir = previous.WorkspaceDir
		if err == nil && cfg.ApplicationMode != "group_mention" && len(policy.Directories) > 0 {
			declaredWorkspace, err = restoredOwnerWorkspace(previous)
		}
	} else if cfg.ApplicationMode != "group_mention" && len(policy.Directories) > 0 {
		prepared, prepareErr := prepareOwnerDirectories(ctx, s.Home, task, attempt.ID, workspace, policy)
		err = prepareErr
		declaredWorkspace = &prepared
		workdir, branch, base = prepared.WorkDir, prepared.Branch, prepared.Base
	} else {
		workdir, branch, base, err = PrepareWorkspace(ctx, s.Home, task, attempt.ID, workspaceForAttempt)
	}
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	err = s.mutate(ctx, "global", "runtime.attempt.workspace", func(tx *core.Tx) (any, error) { return nil, tx.SetRuntimeAttemptWorkspace(ctx, attempt.ID, workdir) })
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	task, err = core.ReadRuntimeTask(ctx, s.Store.DB, task.ID)
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	var snapshots []DirectorySnapshot
	var directoryWriteRoots []string
	if declaredWorkspace != nil {
		snapshots = declaredWorkspace.Snapshots
		directoryWriteRoots = declaredWorkspace.WriteRoots
	}
	if cfg.ApplicationMode == "group_mention" {
		snapshots, err = stageGroupDirectories(ctx, workdir, policy.Directories)
		if err == nil {
			err = os.MkdirAll(filepath.Join(workdir, "artifacts"), 0700)
		}
		if err != nil {
			s.failTask(ctx, cfg, task, attempt, err)
			return
		}
	}
	if err = s.checkTaskAgent(ctx, cfg, task, policy, preset); err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	// Group and declared-directory Agents work from isolated snapshots. Do not
	// disclose the original project path to those processes: it is not mounted
	// in their work directory and could become an accidental host path oracle.
	executionWorkspacePath := workspacePathForExecution(cfg.ApplicationMode, workspace, declaredWorkspace)
	taskConfig := cfg
	taskConfig.AgentCapabilities, taskConfig.MemoryScope = policy.Capabilities, policy.MemoryScope
	memoryContext, err := s.runtimeMemoryContext(ctx, taskConfig, task, workspace)
	if err != nil && core.ErrorCode(err) != "not_found" {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	hotwordContext, err := s.runtimeHotwordContext(ctx, taskConfig, task, workspace)
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	var conversationContext []core.RuntimeMessage
	if hasAgentCapability(policy.Capabilities, "conversation_history_read") {
		conversationContext, err = core.RuntimeTaskConversationContext(ctx, s.Store.DB, cfg, task, 30)
	}
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	if err = core.RuntimeAttemptPolicyCurrent(ctx, s.Store.DB, attempt.ID, task.ID, task.Version); err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	channelPrompt, err := s.runtimeChannelSystemPrompt(ctx, cfg, task)
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	start := s.now()
	var workspaceState json.RawMessage
	if declaredWorkspace != nil {
		workspaceState, _ = json.Marshal(declaredWorkspace)
	}
	var conversationID string
	if cfg.ApplicationMode == "group_mention" || policy.MemoryScope == "conversation_published" {
		route, routeErr := core.ReadRoute(ctx, s.Store.DB, task.RouteID)
		if routeErr != nil {
			s.failTask(ctx, cfg, task, attempt, routeErr)
			return
		}
		conversationID = route.ConversationID
	}
	execInput := ExecutionInput{Task: task, MemoryContext: memoryContext, MemoryScope: policy.MemoryScope, HotwordContext: hotwordContext, ConversationContext: conversationContext,
		AttemptID: attempt.ID, NativeSessionID: attempt.ID, RecordSession: s.sessionRecorder(task, attempt), WorkspaceBranch: branch, WorkspaceBase: base, WorkspaceState: workspaceState, DirectoryPolicy: policy.Directories, AgentPolicyDigest: core.Digest(policy),
		Home: s.Home, AgentHome: effectiveAgentHome(s.Home, policy.Home, policy.Agent), WorkspaceID: workspace.ID, WorkspacePath: executionWorkspacePath, ChannelID: cfg.ChannelID, ConversationID: conversationID,
		WorkDir: workdir, Preset: preset, ApplicationMode: cfg.ApplicationMode, Capabilities: policy.Capabilities, BashEnabled: policy.BashEnabled, ExternalActions: policy.ExternalActions, DirectorySnapshots: snapshots, PolicyResolved: true, ExecutionModel: policy.ExecutionModel, ClaudeProfile: policy.ClaudeProfile, DirectoryBounded: declaredWorkspace != nil, DirectoryWriteRoots: directoryWriteRoots, ChannelSystemPrompt: channelPrompt, Skills: policy.Skills}
	if cfg.ApplicationMode == "proactive" {
		roots := directoryWriteRoots
		if len(roots) == 0 && (policy.BashEnabled || hasAgentCapability(policy.Capabilities, "local_write")) {
			roots = []string{workdir}
		}
		roots, err = core.CanonicalWriteRoots(roots)
		if err == nil {
			err = s.mutate(ctx, "global", "runtime.work.lock", func(tx *core.Tx) (any, error) { return nil, tx.LockWorkRoots(ctx, attempt.ID, roots) })
		}
		if err != nil {
			s.failTask(ctx, cfg, task, attempt, err)
			return
		}
	}
	s.phase(ctx, attempt.ID, "agent")
	result, err := s.Executor.Execute(ctx, execInput)
	if ctx.Err() != nil {
		err = workError(ctx, "execution")
	}
	if err == nil {
		err = s.checkTaskAgent(ctx, cfg, task, policy, preset)
	}
	if err == nil {
		err = core.RuntimeAttemptPolicyCurrent(ctx, s.Store.DB, attempt.ID, task.ID, task.Version)
	}
	if err == nil && cfg.ApplicationMode == "group_mention" {
		result.Artifacts, err = validateGroupArtifacts(workdir, result.Artifacts)
	}
	if err == nil {
		s.phase(ctx, attempt.ID, "validation")
		var commit string
		if declaredWorkspace != nil {
			result.Artifacts, commit, err = finishOwnerDirectories(ctx, *declaredWorkspace, policy, result.Artifacts)
		} else {
			// Investigation may finish without changing a repository. Changed
			// work must still be committed: VerifyWorkspace rejects a dirty tree.
			commit, err = VerifyWorkspace(ctx, workdir, branch, base, false)
		}
		if commit != "" {
			result.Artifacts = append(result.Artifacts, "git:"+commit)
		}
	}
	candidateID := ""
	if err == nil && task.Kind == "memory" && cfg.ApplicationMode != "proactive" {
		s.phase(ctx, attempt.ID, "review")
		reviewCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.ReviewTimeoutSeconds)*time.Second)
		candidateID, err = s.processMemory(reviewCtx, task, result)
		if reviewCtx.Err() != nil {
			err = workError(reviewCtx, "review")
		}
		cancel()
	}
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	var completed core.RuntimeTask
	err = s.mutate(ctx, "global", "runtime.task.complete", func(tx *core.Tx) (any, error) {
		var e error
		completed, e = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, result, candidateID)
		if e == nil && cfg.ApplicationMode == "proactive" && (task.Kind == "memory" || result.Candidate != nil) {
			e = tx.QueueRuntimeReview(ctx, completed, attempt.ID, result.Candidate)
		}
		return completed, e
	})
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: task.ID, AttemptID: attempt.ID, Level: "warn", Component: "execution", Event: "stale_result", ErrorCode: core.ErrorCode(err), Summary: "任务执行期间需求已变化，旧结果未交付"})
		return
	}
	inputTokens, outputTokens, cost := usageFields(result.Usage)
	summary := "任务处理与验证已完成"
	if completed.Status == "blocked" {
		summary = "调查结束，存在待处理阻塞；业务事项尚未完成"
	} else if completed.Status == "awaiting_confirmation" {
		summary = "具体破坏性操作等待 Owner 确认，执行位已释放"
	}
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: task.ID, AttemptID: attempt.ID, Level: "info", Component: "execution", Event: "completed", Status: completed.Status, Model: attempt.Model, ToolKinds: result.ToolKinds, InputTokens: inputTokens, OutputTokens: outputTokens, CostUSD: cost, DurationMS: s.now().Sub(start).Milliseconds(), Summary: summary})
	s.deliver(ctx, cfg, task.ID)
	if completed.Status == "completed" {
		s.completionAcknowledgement(ctx, cfg, task.ID)
	}
}

func workspacePathForExecution(applicationMode string, workspace core.Workspace, declared *OwnerDirectoryWorkspace) string {
	if applicationMode == "group_mention" || declared != nil {
		return ""
	}
	return workspace.Path
}

func (s *Service) runtimeMemoryContext(ctx context.Context, cfg core.RuntimeConfig, task core.RuntimeTask, workspace core.Workspace) (string, error) {
	// Agents choose when to query memory. Private and group turns must not
	// preload memgov memory on every message; the explicit memory skill remains
	// available when the Agent decides it is relevant. Observations are evidence
	// for a separate task, never a request to preload the owner's private history.
	if cfg.ApplicationMode == "direct" || cfg.ApplicationMode == "group_mention" || cfg.ApplicationMode == "proactive" {
		return "", nil
	}
	if !hasAgentCapability(cfg.AgentCapabilities, "memory_read") {
		return "", nil
	}
	if cfg.MemoryScope != "conversation_published" {
		query := task.QueryText()
		recall, err := core.Recall(ctx, s.Store.DB, query, core.SearchOptions{Scope: workspace.ID, Limit: 10}, 12000, false)
		return recall.Context, err
	}
	route, err := core.ReadRoute(ctx, s.Store.DB, task.RouteID)
	if err != nil {
		return "", err
	}
	audience, err := core.AudienceFor(ctx, s.Store.DB, cfg.ChannelID, route.ConversationID)
	if err != nil {
		return "", err
	}
	query := task.QueryText()
	if cfg.ApplicationMode == "group_mention" {
		query = task.Title
	}
	value, err := core.AudienceRecall(ctx, s.Store.DB, audience, query, 12000, false)
	if err != nil {
		return "", err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return "", core.Fail("internal", "audience recall returned an invalid result")
	}
	contextValue, _ := result["context"].(string)
	return contextValue, nil
}

func (s *Service) runtimeHotwordContext(ctx context.Context, cfg core.RuntimeConfig, task core.RuntimeTask, workspace core.Workspace) (string, error) {
	// Hotwords are also memgov memory. Do not perform an implicit lookup for
	// private or group conversations; an Agent may use the explicit memory tool.
	if cfg.ApplicationMode == "direct" || cfg.ApplicationMode == "group_mention" {
		return "", nil
	}
	if !hasAgentCapability(cfg.AgentCapabilities, "memory_read") {
		return "", nil
	}
	if cfg.MemoryScope != "conversation_published" {
		return core.HotwordContext(ctx, s.Store.DB, workspace.ID, nil, 4000)
	}
	route, err := core.ReadRoute(ctx, s.Store.DB, task.RouteID)
	if err != nil {
		return "", err
	}
	audience, err := core.AudienceFor(ctx, s.Store.DB, cfg.ChannelID, route.ConversationID)
	if err != nil {
		return "", err
	}
	return core.HotwordContext(ctx, s.Store.DB, workspace.ID, &audience, 4000)
}

func (s *Service) processMemory(ctx context.Context, task core.RuntimeTask, result core.RuntimeAttemptResult) (string, error) {
	if result.Candidate == nil {
		return "", core.Fail("invalid_input", "memory task did not return a candidate")
	}
	var candidate core.Candidate
	route, err := core.ReadRoute(ctx, s.Store.DB, task.RouteID)
	if err != nil {
		return "", err
	}
	err = s.mutate(ctx, route.WorkspaceID, "runtime.memory.candidate", func(tx *core.Tx) (any, error) {
		var e error
		candidate, e = tx.SubmitCandidate(ctx, *result.Candidate, "", "runtime-executor")
		return candidate, e
	})
	if err != nil {
		return "", err
	}
	review, err := s.Reviewer.Review(ctx, task, *result.Candidate)
	if err != nil {
		return candidate.ID, err
	}
	err = s.mutate(ctx, route.WorkspaceID, "runtime.memory.review", func(tx *core.Tx) (any, error) {
		c, e := tx.SubmitCandidateReview(ctx, candidate.ID, "runtime-reviewer:"+task.ID, review.Decision, review.Issues)
		if e != nil {
			return c, e
		}
		if review.Decision == "accept" {
			return tx.ApplyCandidate(ctx, candidate.ID, c.Digest)
		}
		return c, nil
	})
	return candidate.ID, err
}

func (s *Service) failTask(ctx context.Context, cfg core.RuntimeConfig, task core.RuntimeTask, attempt core.RuntimeAttempt, err error) {
	if ctx.Err() != nil {
		err = workError(ctx, "execution")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	failErr := s.mutate(ctx, "global", "runtime.task.fail", func(tx *core.Tx) (any, error) {
		return tx.FailRuntimeTask(ctx, task.ID, task.Version, attempt.ID, core.ErrorCode(err))
	})
	var duration int64
	if started, parseErr := time.Parse(time.RFC3339Nano, attempt.StartedAt); parseErr == nil && !started.After(s.now()) {
		duration = s.now().Sub(started).Milliseconds()
	}
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: task.ID, AttemptID: attempt.ID, Level: "error", Component: "execution", Event: "failed", DurationMS: duration, Model: attempt.Model, ErrorCode: core.ErrorCode(err), Summary: "任务执行失败，可检查后重试"})
	if failErr == nil {
		s.failAcknowledgement(ctx, cfg, task.ID)
		return
	}
	// A CLI cancellation changes the task version and marks the running
	// attempt stale before the worker observes context cancellation. The
	// worker cannot overwrite that durable state with FailRuntimeTask, but it
	// still owes the owner a terminal failure reaction and safe explanation.
	if current, readErr := core.ReadRuntimeTask(ctx, s.Store.DB, task.ID); readErr == nil && current.Status == "cancelled" && current.Version != task.Version {
		s.failAcknowledgement(ctx, cfg, task.ID)
	}
}

// Message reactions use a durable receipt outbox and never count as an answer.
func (s *Service) acknowledge(ctx context.Context, cfg core.RuntimeConfig, taskID string) {
	s.deliverStage(ctx, cfg, taskID, core.RuntimeReceiptPurpose)
}

func (s *Service) processingAcknowledgement(ctx context.Context, cfg core.RuntimeConfig, taskID string) {
	if cfg.ApplicationMode == "direct" || cfg.ApplicationMode == "group_mention" {
		s.deliverStage(ctx, cfg, taskID, core.RuntimeProcessingReceiptPurpose)
	}
}

func (s *Service) completionAcknowledgement(ctx context.Context, cfg core.RuntimeConfig, taskID string) {
	if cfg.ApplicationMode == "direct" || cfg.ApplicationMode == "group_mention" {
		s.deliverStage(ctx, cfg, taskID, core.RuntimeCompletionReceiptPurpose)
	}
}

func (s *Service) failureStageAcknowledgement(ctx context.Context, cfg core.RuntimeConfig, taskID string) {
	if cfg.ApplicationMode == "direct" || cfg.ApplicationMode == "group_mention" {
		s.deliverStage(ctx, cfg, taskID, core.RuntimeFailureReceiptPurpose)
	}
}

// Stage reactions are user feedback, not a prerequisite for Agent execution.
// The runtime queues them in order and starts useful work immediately. SQLite
// task truth lets startup reconciliation recreate any reaction not sent before
// a crash. Tests and standalone helpers without a live queue stay synchronous.
func (s *Service) deliverStage(ctx context.Context, cfg core.RuntimeConfig, taskID, purpose string) {
	if s.stageQueue == nil {
		s.deliverPurpose(ctx, cfg, taskID, purpose)
		return
	}
	// Persist the outbox row before returning to Agent work. The platform call
	// stays asynchronous, while a crash before the worker consumes this request
	// still leaves enough truth for startup reconciliation.
	out, err := s.preparePurpose(ctx, taskID, purpose)
	if err != nil || out.State != "ready" {
		return
	}
	select {
	case s.stageQueue <- stageDelivery{Config: cfg, TaskID: taskID, Purpose: purpose}:
	case <-ctx.Done():
	}
}

func (s *Service) runStageDeliveries(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-s.stageQueue:
			s.deliverPurpose(ctx, request.Config, request.TaskID, request.Purpose)
		}
	}
}

func (s *Service) failAcknowledgement(ctx context.Context, cfg core.RuntimeConfig, taskID string) {
	s.failureStageAcknowledgement(ctx, cfg, taskID)
	if cfg.ApplicationMode == "direct" || cfg.ApplicationMode == "group_mention" {
		s.deliverFailureNotice(ctx, cfg, taskID)
	}
}

func (s *Service) reconcileStageAcknowledgements(ctx context.Context, cfg core.RuntimeConfig) {
	if cfg.ApplicationMode != "direct" && cfg.ApplicationMode != "group_mention" {
		return
	}
	stages := []struct {
		purpose string
		deliver func(context.Context, core.RuntimeConfig, string)
	}{
		{core.RuntimeProcessingReceiptPurpose, s.processingAcknowledgement},
		{core.RuntimeCompletionReceiptPurpose, s.completionAcknowledgement},
		{core.RuntimeFailureReceiptPurpose, s.failureStageAcknowledgement},
	}
	for _, stage := range stages {
		taskIDs, err := core.RuntimeStageAcknowledgementTaskIDs(ctx, s.Store.DB, cfg.ID, stage.purpose)
		if err != nil {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, Level: "warn", Component: "delivery", Event: "stage_receipts_scan_failed", ErrorCode: core.ErrorCode(err), Summary: "任务阶段标记对账未完成，后续启动将再次核对"})
			continue
		}
		for _, taskID := range taskIDs {
			stage.deliver(ctx, cfg, taskID)
		}
	}
}

// Group replies, including confirmation details, stay in the triggering group.
func (s *Service) deliver(ctx context.Context, cfg core.RuntimeConfig, taskID string) bool {
	return s.deliverPurpose(ctx, cfg, taskID, "result")
}

func (s *Service) preparePurpose(ctx context.Context, taskID, purpose string) (core.OutboxView, error) {
	var out core.OutboxView
	err := s.mutate(ctx, "global", "runtime.delivery.prepare", func(tx *core.Tx) (any, error) {
		var prepareErr error
		switch purpose {
		case core.RuntimeReceiptPurpose:
			out, prepareErr = tx.PrepareTaskAcknowledgement(ctx, taskID)
		case core.RuntimeProcessingReceiptPurpose:
			out, prepareErr = tx.PrepareTaskProcessingAcknowledgement(ctx, taskID)
		case core.RuntimeCompletionReceiptPurpose:
			out, prepareErr = tx.PrepareTaskCompletionAcknowledgement(ctx, taskID)
		case core.RuntimeFailureReceiptPurpose:
			out, prepareErr = tx.PrepareTaskFailureAcknowledgement(ctx, taskID)
		default:
			out, prepareErr = tx.PrepareTaskDelivery(ctx, taskID)
		}
		return out, prepareErr
	})
	return out, err
}

func (s *Service) deliverPurpose(ctx context.Context, cfg core.RuntimeConfig, taskID, purpose string) bool {
	// Completion is record-only for Cyber owner. Business communication must
	// be an explicit Agent action through the audited owner send interface.
	if cfg.ApplicationMode == "proactive" {
		return true
	}
	c, err := core.ReadChannel(ctx, s.Store.DB, cfg.ChannelID)
	if err != nil {
		return false
	}
	if !c.Capabilities.Verified["send"] {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: taskID, Level: "warn", Component: "delivery", Event: "blocked", ErrorCode: "unavailable", Summary: "机器人发送能力尚未验证"})
		return false
	}
	var reactor channel.Reactor
	isReaction := core.RuntimeReactionPurpose(purpose)
	if isReaction {
		var supported bool
		reactor, supported = s.Adapter.(channel.Reactor)
		if !supported {
			s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: taskID, Level: "warn", Component: "delivery", Event: "blocked", ErrorCode: "unavailable", Summary: "通道不支持消息接收表情"})
			return false
		}
	}
	out, err := s.preparePurpose(ctx, taskID, purpose)
	if err != nil || out.State != "ready" {
		return false
	}
	var attempt int
	err = s.mutate(ctx, "global", "runtime.delivery.begin", func(tx *core.Tx) (any, error) {
		var e error
		attempt, e = tx.BeginDelivery(ctx, out.ID)
		return attempt, e
	})
	if err != nil {
		return false
	}
	target := out.ConversationID
	if out.Transport == "bot_dm" {
		target = cfg.OwnerIDValue
	}
	var result channel.SendResult
	var sendErr error
	if isReaction {
		// Direct delivery uses a user identifier, but reactions require the
		// original platform conversation and message identifiers.
		task, readErr := core.ReadRuntimeTask(ctx, s.Store.DB, taskID)
		if readErr != nil {
			sendErr = readErr
		} else if len(task.Messages) != 1 || task.Messages[0].ProviderMessageID != out.ReplyTo || out.Format != "reaction" {
			sendErr = core.Fail("denied", "reaction must target the original interactive message")
		} else {
			reaction := channel.ReactionRequest{ConversationID: task.Messages[0].ConversationID, MessageID: out.ReplyTo, Emoji: out.Content}
			var cleanupErr error
			if purpose != core.RuntimeReceiptPurpose {
				predecessorPurpose := core.RuntimeReceiptPurpose
				predecessorEmoji := core.RuntimeAcknowledgement
				if purpose == core.RuntimeCompletionReceiptPurpose || purpose == core.RuntimeFailureReceiptPurpose {
					var processingAccepted int
					readErr = s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM outbox WHERE job_id=? AND reason=? AND state='accepted'", taskID, core.RuntimeProcessingReceiptPurpose).Scan(&processingAccepted)
					if readErr == nil && processingAccepted > 0 {
						predecessorPurpose = core.RuntimeProcessingReceiptPurpose
						predecessorEmoji = core.RuntimeProcessingAcknowledgement
					}
				}
				var acknowledged int
				if readErr == nil {
					readErr = s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM outbox WHERE job_id=? AND reason=? AND state='accepted'", taskID, predecessorPurpose).Scan(&acknowledged)
				}
				if readErr != nil {
					cleanupErr = readErr
				} else if acknowledged > 0 {
					cleanupErr = reactor.RemoveReaction(ctx, channel.ConfigFor(c), channel.ReactionRequest{ConversationID: reaction.ConversationID, MessageID: reaction.MessageID, Emoji: predecessorEmoji})
				}
			}
			sendErr = reactor.AddReaction(ctx, channel.ConfigFor(c), reaction)
			if sendErr == nil && cleanupErr != nil {
				sendErr = cleanupErr
			}
			if sendErr == nil {
				result = channel.SendResult{State: "accepted", Receipt: "reaction_added"}
			}
		}
	} else {
		result, sendErr = s.Adapter.Send(ctx, channel.ConfigFor(c), channel.SendRequest{ConversationID: target, Transport: out.Transport, Content: out.Content, Format: out.Format, ReplyTo: out.ReplyTo, IdempotencyKey: out.ID})
	}
	state := result.State
	if sendErr != nil {
		state = "unknown"
		if core.ErrorCode(sendErr) == "denied" {
			state = "failed"
		}
	}
	if !containsState(state) {
		state = "unknown"
	}
	err = s.mutate(ctx, "global", "runtime.delivery.finish", func(tx *core.Tx) (any, error) {
		return nil, tx.FinishDelivery(ctx, out.ID, attempt, state, result.Receipt)
	})
	if err != nil {
		s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: taskID, Level: "error", Component: "delivery", Event: "record_failed", ErrorCode: core.ErrorCode(err), Summary: "机器人投递状态记录失败，结果未标记为交付"})
		return false
	}
	level := "info"
	if state != "accepted" {
		level = "warn"
	}
	deliverySummary := "处理结果已通过机器人私聊交付"
	if out.Transport == "bot_group" {
		deliverySummary = "处理结果已回复触发群"
	}
	if purpose == core.RuntimeReceiptPurpose {
		deliverySummary = "已在原消息添加接收表情"
	} else if purpose == core.RuntimeProcessingReceiptPurpose {
		deliverySummary = "已把原消息状态更新为处理中"
	} else if purpose == core.RuntimeCompletionReceiptPurpose {
		deliverySummary = "已把原消息状态更新为已完成"
	} else if purpose == core.RuntimeFailureReceiptPurpose {
		deliverySummary = "已把原消息接收表情更新为失败标记"
	}
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: taskID, Level: level, Component: "delivery", Event: map[string]string{"result": "finished", core.RuntimeReceiptPurpose: "acknowledged", core.RuntimeProcessingReceiptPurpose: "processing_marked", core.RuntimeCompletionReceiptPurpose: "completed_marked", core.RuntimeFailureReceiptPurpose: "failed_marked"}[purpose], Status: state, ErrorCode: func() string {
		if sendErr != nil {
			return core.ErrorCode(sendErr)
		}
		return ""
	}(), Summary: map[bool]string{true: deliverySummary, false: "处理结果投递未确认成功"}[state == "accepted"]})
	return state == "accepted"
}
func containsState(v string) bool { return v == "accepted" || v == "failed" || v == "unknown" }

func usageFields(usage map[string]any) (int64, int64, float64) {
	number := func(v any) float64 {
		switch n := v.(type) {
		case int:
			return float64(n)
		case int64:
			return float64(n)
		case float64:
			return n
		case json.Number:
			value, _ := n.Float64()
			return value
		default:
			return 0
		}
	}
	return int64(number(usage["input_tokens"])), int64(number(usage["output_tokens"])), number(usage["cost_usd"])
}

func mustSourceRoutes(ids []string) string { b, _ := json.Marshal(ids); return string(b) }

// ReceiveChannel runs the single receiver for all application routes.
func (s *Service) ReceiveChannel(ctx context.Context, c core.Channel) error {
	s.receiveLoop(ctx, core.RuntimeConfig{ID: c.ID}, c)
	return ctx.Err()
}
