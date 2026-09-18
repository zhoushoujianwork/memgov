package channel

import (
	"context"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"sync"
	"time"
)

// Discovery, live receive, bounded reconciliation and historical import never
// wait for each other's network calls. The adapter shares two request slots.
func (s *DataSourceService) startBackgroundMaintenance(ctx context.Context, id string) (func(core.DataSource, bool, bool), <-chan struct{}) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var scope core.DataSource
	var ready, historyReady bool
	update := func(d core.DataSource, allowed, allowHistory bool) {
		mu.Lock()
		defer mu.Unlock()
		scope = d
		ready = allowed
		historyReady = allowHistory
	}
	for _, history := range []bool{false, true} {
		wg.Add(1)
		go func(history bool) {
			defer wg.Done()
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			next := time.Time{}
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				if s.now().Before(next) {
					continue
				}
				next = s.now().Add(time.Second)
				mu.Lock()
				d, allowed := scope, ready
				if history {
					allowed = historyReady
				}
				mu.Unlock()
				if !allowed {
					continue
				}
				cycle, cancel := context.WithTimeout(ctx, 2*time.Minute)
				if history {
					if s.HistoryStep != nil {
						began := time.Now()
						worked, e := s.HistoryStep(cycle, id)
						if e != nil && core.ErrorCode(e) != "not_found" && ctx.Err() == nil {
							s.logEvent(ctx, id, "history_step", "failed", "历史导入步骤失败", began, e)
							next = s.now().Add(5 * time.Second)
						} else if worked && e == nil {
							s.logEvent(ctx, id, "history_step", "complete", "历史导入步骤已提交", began, nil)
						}
					}
				} else {
					conversations, e := s.sourceConversations(cycle, d)
					if e == nil {
						collector := Collector{Store: s.Store, Adapter: s.Adapter, Conversations: conversations, CollectAllDirect: d.DirectEnabled, DirectSourceID: id, EarliestSentAt: s.now().UTC().AddDate(0, 0, -d.RetentionDays)}
						e = s.Reconcile(cycle, d, collector)
					}
					if e != nil && ctx.Err() == nil {
						_ = s.recordError(ctx, id, sourceErrorCode(e))
					}
					next = s.now().Add(sourceReconcileInterval(d))
				}
				cancel()
			}
		}(history)
	}
	go func() { wg.Wait(); close(done) }()
	return update, done
}
