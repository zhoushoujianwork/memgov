package channel

import (
	"context"
	"io"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// DataSourceService owns collection only. AI consumers read committed SQLite
// messages independently, so pausing/stopping a runtime does not affect it.
type DataSourceService struct {
	ExternalLifecycle bool // Preserve module control intent on supervisor cancellation.
	Logger            SourceLogger
	Diagnostic        io.Writer
	logMu             sync.Mutex
	logFailed         bool
	Store             *core.Store
	Adapter           Adapter
	Tick              time.Duration
	HistoryStep       func(context.Context, string) (bool, error)
	Now               func() time.Time
}

func (s *DataSourceService) mutate(ctx context.Context, command string, fn func(*core.Tx) (any, error)) error {
	_, err := s.Store.Mutate(ctx, core.Request{Scope: "global", Command: command, Actor: "data-source"}, fn)
	return err
}
func (s *DataSourceService) Run(ctx context.Context, value string) error {
	d, err := core.ReadDataSource(ctx, s.Store.DB, value)
	if err != nil {
		return err
	}
	c, err := core.ReadChannel(ctx, s.Store.DB, d.ChannelID)
	if err != nil {
		return err
	}
	if s.Adapter == nil || !c.Capabilities.Verified["receive"] || !c.Capabilities.Verified["history"] {
		return core.Fail("unavailable", "data source requires verified receive and history capabilities")
	}
	lease, err := core.ReadLease(ctx, s.Store.DB, c.ID)
	if err != nil {
		return err
	}
	if lease.Held {
		return core.Fail("conflict", "channel already has a receiver; stop legacy runtime before source start")
	}
	err = s.mutate(ctx, "data-source.start", func(tx *core.Tx) (any, error) { return tx.SetDataSourceStatus(ctx, d.ID, "running") })
	if err != nil {
		return err
	}
	if s.Logger != nil {
		_ = s.mutate(ctx, "data-source.logging.healthy", func(tx *core.Tx) (any, error) { return nil, tx.SetDataSourceLogHealth(ctx, d.ID, false) })
	}
	s.logEvent(ctx, d.ID, "started", "running", "独立采集已启动", time.Time{}, nil)
	lastStatus := "running"
	tick := s.Tick
	if tick <= 0 {
		tick = 250 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	var cancel context.CancelFunc
	var done chan error
	stopSession := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
			done = nil
		}
	}
	defer stopSession()
	next := time.Time{}
	for {
		current, readErr := core.ReadDataSource(ctx, s.Store.DB, d.ID)
		if readErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return readErr
		}
		if current.Status != lastStatus {
			s.logEvent(ctx, d.ID, "status_changed", current.Status, "采集控制状态已更新", time.Time{}, nil)
			lastStatus = current.Status
		}
		if current.Status == "stopped" {
			return nil
		}
		if current.Status == "paused" {
			stopSession()
			next = time.Time{}
		}
		if current.Status == "running" && cancel == nil && !time.Now().Before(next) {
			sessionCtx, cancelSession := context.WithCancel(ctx)
			cancel = cancelSession
			done = make(chan error, 1)
			go func() { done <- s.session(sessionCtx, current, c) }()
			next = time.Now().Add(time.Second)
		}
		select {
		case <-ctx.Done():
			stopSession()
			if !s.ExternalLifecycle {
				_ = s.mutate(context.Background(), "data-source.stop", func(tx *core.Tx) (any, error) { return tx.SetDataSourceStatus(context.Background(), d.ID, "stopped") })
			}
			s.logEvent(context.Background(), d.ID, "stopped", "stopped", "独立采集已停止", time.Time{}, nil)
			return nil
		case err := <-done:
			cancel()
			cancel = nil
			done = nil
			if err != nil {
				s.logEvent(ctx, d.ID, "receiver_retry", "retrying", "采集连接中断，准备重试："+err.Error(), time.Time{}, err)
				_ = s.recordError(ctx, d.ID, sourceErrorCode(err))
				// A competing receiver is a terminal ownership conflict, not a reconnect.
				if core.ErrorCode(err) == "conflict" {
					return err
				}
				next = time.Now().Add(time.Second)
			}
		case <-ticker.C:
		}
	}
}
func (s *DataSourceService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func sourceReconcileInterval(d core.DataSource) time.Duration {
	// Persisted input is validated too; guard old/corrupt state from a hot loop.
	return time.Duration(max(10, d.ReconcileSeconds)) * time.Second
}

// refreshGroups records only fresh verified provider observations. Partial
// observations preserve old routes without promoting them to fresh proof.
func (s *DataSourceService) refreshGroups(ctx context.Context, d core.DataSource, c core.Channel) (core.DataSource, error) {
	began := time.Now()
	observation := GroupDiscovery{Complete: true}
	discoveryConfig := ConfigFor(c)
	if d.MemberRobotCode == "" {
		discoveryConfig.Identity.DeliveryRobotCode = ""
		discoveryConfig.Identity.DeliveryRobotName = ""
	}
	var err error
	if detailed, ok := s.Adapter.(DetailedGroupLister); ok {
		observation, err = detailed.DiscoverGroupConversations(ctx, discoveryConfig)
	} else if lister, ok := s.Adapter.(GroupLister); ok {
		observation.Groups, err = lister.ListGroupConversations(ctx, discoveryConfig)
	} else {
		return d, nil
	}
	if err == nil && !observation.Complete && len(observation.Groups) == 0 {
		err = core.Fail("unavailable", "group discovery has no verified positive observations")
	}
	groups := observation.Groups
	if err == nil {
		ids := make([]core.DataSourceGroup, 0, len(groups))
		for _, g := range groups {
			ids = append(ids, core.DataSourceGroup{ID: g.ID, Name: g.Name})
		}
		err = s.mutate(ctx, "data-source.groups", func(tx *core.Tx) (any, error) {
			current, e := core.ReadDataSource(ctx, tx.Conn, d.ID)
			if e != nil {
				return nil, e
			}
			if current.Status != "running" || current.Version != d.Version {
				return nil, core.Fail("conflict", "source changed during group discovery")
			}
			next, e := tx.ApplySourceGroupObservation(ctx, d.ID, ids, observation.Complete, observation.Excluded)
			if e != nil {
				return nil, e
			}
			if e = tx.RecordSourceGroupObservation(ctx, d.ID, d.Version, c.ConfigVersion, ids, observation.Complete); e != nil {
				return nil, e
			}
			d = next
			return d, nil
		})
	}
	if err != nil {
		if ctx.Err() != nil {
			return d, ctx.Err()
		}
		if e := s.mutate(ctx, "data-source.discovery-invalid", func(tx *core.Tx) (any, error) {
			if core.ErrorCode(err) != "unavailable" {
				return nil, tx.RevokeSourceGroupDiscovery(ctx, d.ID)
			}
			return nil, tx.InvalidateSourceGroupDiscovery(ctx, d.ID)
		}); e != nil {
			return d, e
		}
		if e := s.recordError(ctx, d.ID, sourceErrorCode(err)); e != nil {
			return d, e
		}
		s.logEvent(ctx, d.ID, "group_sync_failed", "failed", "群范围同步失败，发现收据已失效", began, err)
		return d, err
	}
	status := "complete"
	if !observation.Complete {
		status = "partial"
		_ = s.recordError(ctx, d.ID, "discovery_partial")
	} else {
		_ = s.recordError(ctx, d.ID, "")
	}
	s.logEvent(ctx, d.ID, "groups_synced", status, sourceCountSummary(len(observation.Groups)), began, nil)
	return d, nil
}
func (s *DataSourceService) sourceConversations(ctx context.Context, d core.DataSource) ([]string, error) {
	conversations := []string{}
	for _, id := range d.RouteIDs {
		r, err := core.ReadRoute(ctx, s.Store.DB, id)
		if err != nil {
			return nil, err
		}
		if (r.ConversationType == "group" || r.ConversationType == "direct") && r.Status == "active" && r.Mode != "ignore" {
			conversations = append(conversations, r.ConversationID)
		}
	}
	sort.Strings(conversations)
	return conversations, nil
}

func directWindowStart(d core.DataSource, now time.Time) time.Time {
	start := now.UTC().AddDate(0, 0, -d.RetentionDays)
	if enabled, err := time.Parse(time.RFC3339, d.DirectEnabledAt); err == nil && enabled.After(start) {
		start = enabled
	}
	return start
}

// refreshDirect discovers only messages at or after the explicit enable point.
// Display names are retained as lookup hints and never become identities.
func (s *DataSourceService) refreshDirect(ctx context.Context, d core.DataSource, c core.Channel) (core.DataSource, bool, error) {
	if !d.DirectEnabled {
		return d, true, nil
	}
	lister, ok := s.Adapter.(DirectConversationLister)
	if !ok {
		return d, false, core.Fail("unavailable", "adapter does not support bounded direct conversation discovery")
	}
	start, end := directWindowStart(d, s.now()), s.now().UTC()
	if covered, parseErr := time.Parse(time.RFC3339, d.DirectDiscoveryCoveredUntil); parseErr == nil && covered.After(start) {
		start = covered
	}
	if end.After(start.Add(time.Hour)) {
		end = start.Add(time.Hour)
	}
	if !end.After(start) {
		return d, true, nil
	}
	items, complete, reason, err := lister.ListDirectConversations(ctx, ConfigFor(c), start, end, 40, 1000)
	if err != nil {
		return d, false, err
	}
	for _, item := range items {
		err = s.mutate(ctx, "data-source.direct-route", func(tx *core.Tx) (any, error) {
			return tx.AdmitDataSourceDirectConversation(ctx, d.ID, item.ID, item.DisplayName)
		})
		if err != nil {
			return d, false, err
		}
	}
	d, err = core.ReadDataSource(ctx, s.Store.DB, d.ID)
	if err != nil {
		return d, false, err
	}
	if complete {
		err = s.mutate(ctx, "data-source.direct-coverage", func(tx *core.Tx) (any, error) {
			_, updateErr := tx.Conn.ExecContext(ctx, "UPDATE data_sources SET direct_discovery_covered_until=?,updated_at=? WHERE id=?", end.Format(time.RFC3339Nano), core.Now(), d.ID)
			return nil, updateErr
		})
		if err != nil {
			return d, false, err
		}
		d.DirectDiscoveryCoveredUntil = end.Format(time.RFC3339Nano)
	}
	if !complete {
		if reason == "" {
			reason = "direct_discovery_partial"
		}
		_ = s.recordError(ctx, d.ID, reason)
	}
	return d, complete, nil
}

// session has one receive goroutine and one serial maintenance worker. Healthy
// unchanged subscriptions retain their lease across reconciliation cycles;
// scope changes cancel and join the old subscription before opening a new one.
func (s *DataSourceService) session(ctx context.Context, d core.DataSource, c core.Channel) error {
	ctx, cancelSession := context.WithCancel(ctx)
	updateMaintenance, backgroundDone := s.startBackgroundMaintenance(ctx, d.ID)
	defer func() { cancelSession(); <-backgroundDone }()
	var receiveCancel context.CancelFunc
	var receiveDone chan struct{}
	var receiveErr error
	var subscribed []string
	stopReceiver := func() {
		if receiveCancel != nil {
			receiveCancel()
			<-receiveDone
			receiveCancel = nil
			receiveDone = nil
		}
	}
	defer stopReceiver()
	startReceiver := func(conversations []string) error {
		stopReceiver()
		subscribed = append([]string{}, conversations...)
		if len(conversations) == 0 && !d.DirectEnabled {
			return nil
		}
		earliest := s.now().UTC().AddDate(0, 0, -d.RetentionDays)
		sourceID := d.ID
		collector := Collector{Store: s.Store, Adapter: s.Adapter, Conversations: conversations, CollectAllDirect: d.DirectEnabled, DirectSourceID: d.ID, EarliestSentAt: earliest, OnReady: func(map[string]any) {
			s.logEvent(ctx, sourceID, "receiver_ready", "listening", "DWS 实时消费者已就绪", time.Time{}, nil)
		}}
		actor := "data-source:" + d.ID + ":" + core.NewID()
		receiveCtx, cancel := context.WithCancel(ctx)
		receiveCancel = cancel
		receiveDone = make(chan struct{})
		receiveErr = nil
		done := receiveDone
		go func() {
			_, receiveErr = collector.Receive(receiveCtx, core.Request{Scope: "global", Actor: actor}, c.ID, 30*time.Second)
			close(done)
		}()
		for {
			lease, err := core.ReadLease(ctx, s.Store.DB, c.ID)
			if err != nil {
				return err
			}
			if lease.Held && lease.Holder == actor+"@receiver" {
				break
			}
			select {
			case <-done:
				return receiveErr
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
		s.logEvent(ctx, d.ID, "receiver_leased", "running", "实时采集接收租约已建立", time.Time{}, nil)
		return nil
	}
	tick := s.Tick
	if tick <= 0 {
		tick = 250 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	// A valid persisted scope can receive immediately while discovery refreshes.
	if prior, e := core.ReadSourceFallbackScope(ctx, s.Store.DB, d.ID, s.now()); e == nil {
		updateMaintenance(prior, true, false)
		if conversations, e := s.sourceConversations(ctx, prior); e == nil {
			if e = startReceiver(conversations); e != nil {
				return e
			}
		}
	}
	nextReconcile, nextRetention := time.Time{}, time.Time{}
	admitted := false
	discoveryDegraded := false
	for {
		if ctx.Err() != nil {
			return nil
		}
		current, err := core.ReadDataSource(ctx, s.Store.DB, d.ID)
		if err != nil {
			return err
		}
		if current.Status != "running" {
			return nil
		}
		d = current
		now := s.now()
		if !now.Before(nextReconcile) {
			maintenanceCtx, maintenanceCancel := context.WithTimeout(ctx, 2*time.Minute)
			s.maintainLogs(ctx, d.ID)
			c, err = core.ReadChannel(ctx, s.Store.DB, d.ChannelID)
			if err != nil {
				maintenanceCancel()
				return err
			}
			refreshed, discoveryErr := s.refreshGroups(maintenanceCtx, d, c)
			discoveryDegraded = discoveryErr != nil
			if discoveryErr == nil {
				d = refreshed
				admitted = true
			} else {
				admitted = false
				if core.ErrorCode(discoveryErr) == "unavailable" {
					fallback, fallbackErr := core.ReadSourceFallbackScope(ctx, s.Store.DB, d.ID, s.now())
					if fallbackErr == nil {
						d, admitted = fallback, true
						s.logEvent(ctx, d.ID, "discovery_degraded", "degraded", "群发现暂时不可用，继续采集既有授权且证据有效的会话", time.Time{}, discoveryErr)
					}
				}
				if !admitted {
					stopReceiver()
				}
			}
			// Fallback is an in-memory intersection of existing routes and prior
			// proof. It never writes a route, receipt, or new observation timestamp.
			directReady := !d.DirectEnabled
			if d.DirectEnabled {
				base := d
				if admitted {
					base = refreshed
				}
				var directComplete bool
				var directErr error
				base, directComplete, directErr = s.refreshDirect(maintenanceCtx, base, c)
				if directErr == nil {
					d, directReady = base, true
					if !directComplete {
						_ = s.recordError(ctx, d.ID, "direct_discovery_partial")
					}
				} else {
					_ = s.recordError(ctx, d.ID, "direct_"+sourceErrorCode(directErr))
					s.logEvent(ctx, d.ID, "direct_sync_failed", "failed", "私聊发现失败；群采集继续运行", time.Time{}, directErr)
				}
			}
			if admitted || directReady {
				conversations, e := s.sourceConversations(ctx, d)
				if e != nil {
					maintenanceCancel()
					return e
				}
				if !slices.Equal(conversations, subscribed) || receiveCancel == nil {
					if e = startReceiver(conversations); e != nil {
						maintenanceCancel()
						return e
					}
				}
			}
			if discoveryDegraded {
				// Reconciliation checkpoints must not hide the independent discovery failure.
				_ = s.recordError(ctx, d.ID, "discovery_"+sourceErrorCode(discoveryErr))
			}
			// Schedule after work completes: no overlapping cycles or catch-up burst.
			nextReconcile = s.now().Add(sourceReconcileInterval(d))
			updateMaintenance(d, admitted || d.DirectEnabled, admitted && !discoveryDegraded)
			maintenanceCancel()
		}
		if !s.now().Before(nextRetention) {
			var cleaned core.RetentionResult
			_, e := s.Store.Mutate(ctx, core.Request{Scope: "global", Command: "data-source.retention", Actor: "data-source"}, func(tx *core.Tx) (any, error) {
				var err error
				cleaned, err = tx.ApplyDataSourceRetention(ctx, d.ID, s.now(), 50)
				return cleaned, err
			})
			if e != nil {
				_ = s.recordError(ctx, d.ID, "retention_"+sourceErrorCode(e))
				_ = s.mutate(ctx, "data-source.retention-error", func(tx *core.Tx) (any, error) {
					_, updateErr := tx.Conn.ExecContext(ctx, "UPDATE data_sources SET retention_error_code=?,updated_at=? WHERE id=?", sourceErrorCode(e), core.Now(), d.ID)
					return nil, updateErr
				})
			}
			nextRetention = s.now().Add(time.Hour)
			if e == nil && cleaned.Expired == 50 {
				nextRetention = s.now().Add(time.Second) // drain backlog with separate bounded transactions
			}
		}
		select {
		case <-receiveDone:
			return receiveErr
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Reconcile advances a persisted round-robin cursor. Each existing Collector
// pull persists messages and coverage independently and bounds provider work.
func (s *DataSourceService) Reconcile(ctx context.Context, d core.DataSource, collector Collector) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	began := time.Now()
	defer func() {
		status := "complete"
		if resultErr != nil {
			status = "partial"
		}
		s.logEvent(context.WithoutCancel(ctx), d.ID, "reconciled", status, "有界并发补漏检查结束", began, resultErr)
	}()
	if len(d.RouteIDs) == 0 {
		return nil
	}
	limit := min(20, len(d.RouteIDs))
	jobs := make(chan int, 20)
	results := make(chan struct {
		offset int
		err    error
	}, 20)
	for i := 0; i < limit; i++ {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for n := 0; n < 2; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for offset := range jobs {
				if ctx.Err() != nil {
					return
				}
				index := (d.ReconcileCursor + offset) % len(d.RouteIDs)
				routeCtx, stop := context.WithTimeout(ctx, 30*time.Second)
				err := s.reconcileRoute(routeCtx, d, collector, d.RouteIDs[index])
				stop()
				results <- struct {
					offset int
					err    error
				}{offset, err}
			}
		}()
	}
	go func() { workers.Wait(); close(results) }()
	completed := make([]bool, limit)
	contiguous := 0
	var lastErr error
	for result := range results {
		completed[result.offset] = true
		if result.err != nil {
			lastErr = result.err
		}
		for contiguous < limit && completed[contiguous] {
			contiguous++
		}
		checkpointCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		code := ""
		if lastErr != nil {
			code = sourceErrorCode(lastErr)
		}
		err := s.mutate(checkpointCtx, "data-source.checkpoint", func(tx *core.Tx) (any, error) {
			return nil, tx.CheckpointDataSource(checkpointCtx, d.ID, (d.ReconcileCursor+contiguous)%len(d.RouteIDs), code)
		})
		stop()
		if err != nil {
			lastErr = err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return lastErr
}

func (s *DataSourceService) reconcileRoute(ctx context.Context, d core.DataSource, collector Collector, routeID string) (resultErr error) {
	var deferred int
	if err := s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM source_route_backoff WHERE source_id=? AND route_id=? AND julianday(next_run_at)>julianday(?)", d.ID, routeID, s.now().UTC().Format(time.RFC3339Nano)).Scan(&deferred); err != nil {
		return err
	}
	if deferred > 0 {
		return nil
	}
	defer func() {
		// A normal service stop is not a failing conversation. Preserve any
		// prior backoff, but don't penalize restart because cancellation raced
		// with a successful provider response/checkpoint.
		if ctx.Err() == context.Canceled {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		e := s.mutate(cleanup, "data-source.route.backoff", func(tx *core.Tx) (any, error) {
			if resultErr == nil {
				_, err := tx.Conn.ExecContext(cleanup, "DELETE FROM source_route_backoff WHERE source_id=? AND route_id=?", d.ID, routeID)
				return nil, err
			}
			var failures int
			_ = tx.Conn.QueryRowContext(cleanup, "SELECT failures FROM source_route_backoff WHERE source_id=? AND route_id=?", d.ID, routeID).Scan(&failures)
			delay := time.Duration(min(300, 5*(1<<min(failures, 6)))) * time.Second
			_, err := tx.Conn.ExecContext(cleanup, `INSERT INTO source_route_backoff(source_id,route_id,failures,next_run_at,error_code) VALUES(?,?,?,?,?) ON CONFLICT(source_id,route_id) DO UPDATE SET failures=excluded.failures,next_run_at=excluded.next_run_at,error_code=excluded.error_code`, d.ID, routeID, failures+1, s.now().Add(delay).UTC().Format(time.RFC3339Nano), sourceErrorCode(resultErr))
			return nil, err
		})
		if e != nil {
			resultErr = e
		}
	}()
	route, err := core.ReadRoute(ctx, s.Store.DB, routeID)
	if err == nil && (route.ConversationType == "group" || (route.ConversationType == "direct" && d.DirectEnabled && d.BackfillAfterEnable)) && route.Status == "active" && route.Mode != "ignore" {
		var start, end time.Time
		cursor := ""
		var gapStart, gapEnd string
		gapErr := s.Store.DB.QueryRowContext(ctx, "SELECT start_at,end_at,cursor FROM coverage_windows WHERE channel_id=? AND conversation_id=? AND complete=0 AND resolved_at='' AND cursor<>'' ORDER BY created_at DESC LIMIT 1", d.ChannelID, route.ConversationID).Scan(&gapStart, &gapEnd, &cursor)
		if gapErr == nil {
			start, err = time.Parse(time.RFC3339, gapStart)
			if err == nil {
				end, err = time.Parse(time.RFC3339, gapEnd)
			}
		} else {
			start, end, err = core.NextWindow(ctx, s.Store.DB, d.ChannelID, route.ConversationID, s.now(), time.Hour, 5*time.Minute)
		}
		if err == nil {
			minimum := s.now().UTC().AddDate(0, 0, -d.RetentionDays)
			if route.ConversationType == "direct" {
				if enabled, parseErr := time.Parse(time.RFC3339, d.DirectEnabledAt); parseErr == nil && enabled.After(minimum) {
					minimum = enabled
				}
			}
			if cursor == "" && start.Before(minimum) {
				start = minimum
			}
			if route.ConversationType == "direct" && cursor == "" {
				w, _ := core.ReadWatermark(ctx, s.Store.DB, d.ChannelID, route.ConversationID)
				if w.CoveredUntil == "" {
					start = minimum
				}
			}
			_, err = collector.PullCursor(ctx, core.Request{Scope: route.WorkspaceID, Actor: "data-source:" + d.ID}, d.ChannelID, route.ConversationID, start, end, cursor, 5, 100)
		}
	}

	return err
}

func (s *DataSourceService) recordError(ctx context.Context, id, code string) error {
	return s.mutate(ctx, "data-source.error", func(tx *core.Tx) (any, error) {
		_, err := tx.Conn.ExecContext(ctx, "UPDATE data_sources SET last_error_code=?,updated_at=? WHERE id=?", code, core.Now(), id)
		return nil, err
	})
}
