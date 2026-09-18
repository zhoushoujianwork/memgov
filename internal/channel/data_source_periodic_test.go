package channel

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type periodicSourceAdapter struct {
	fakeAdapter
	mu                                       sync.Mutex
	groups                                   []GroupConversation
	discoveryErr                             error
	historyErr                               error
	discoveries, reads, starts, active, peak int
	readConversations                        []string
	scope                                    []string
}

func (a *periodicSourceAdapter) ListGroupConversations(context.Context, Config) ([]GroupConversation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.discoveries++
	return append([]GroupConversation{}, a.groups...), a.discoveryErr
}
func (a *periodicSourceAdapter) ReadWindow(ctx context.Context, c Config, w Window) (WindowResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	a.readConversations = append(a.readConversations, w.ConversationID)
	return WindowResult{Complete: a.historyErr == nil}, a.historyErr
}
func (a *periodicSourceAdapter) RunReceiver(ctx context.Context, c Config, _ ReceiverOptions) error {
	a.mu.Lock()
	a.starts++
	a.active++
	a.peak = max(a.peak, a.active)
	a.scope = append([]string{}, c.Conversations...)
	a.mu.Unlock()
	<-ctx.Done()
	a.mu.Lock()
	a.active--
	a.mu.Unlock()
	return nil
}
func (a *periodicSourceAdapter) counts() (int, int, int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.discoveries, a.reads, a.starts, a.peak
}
func sourceEventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("source condition was not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func periodicFixture(t *testing.T, a *periodicSourceAdapter) (Collector, core.DataSource) {
	t.Helper()
	collector, c := testCollector(t, a)
	ctx := context.Background()
	var d core.DataSource
	_, err := collector.Store.Mutate(ctx, core.Request{Scope: "global", Command: "periodic.fixture"}, func(tx *core.Tx) (any, error) {
		c.Identity.DeliveryRobotCode = "robot"
		c.Identity.DeliveryRobotName = "member bot"
		if _, e := tx.Conn.ExecContext(ctx, "UPDATE channels SET identity=? WHERE id=?", core.JSON(c.Identity), c.ID); e != nil {
			return nil, e
		}
		if _, e := tx.SetChannelCapabilities(ctx, c.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); e != nil {
			return nil, e
		}
		var e error
		d, e = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "periodic", Channel: c.ID, Workspace: "global", MemberRobotCode: "robot", ReconcileSeconds: 10})
		return d, e
	})
	if err != nil {
		t.Fatal(err)
	}
	return collector, d
}
func TestDataSourcePeriodicReconcileKeepsHealthyLeaseAndInvalidatesFailedDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := &periodicSourceAdapter{groups: []GroupConversation{{ID: "cid:group1", Name: "group"}}}
	collector, d := periodicFixture(t, adapter)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	service := DataSourceService{Store: collector.Store, Adapter: adapter, Tick: time.Millisecond, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx, d.ID) }()
	sourceEventually(t, func() bool { _, reads, starts, _ := adapter.counts(); return reads == 1 && starts == 1 })
	first, _ := core.ReadLease(ctx, collector.Store.DB, d.ChannelID)
	initial, err := core.ReadSourceGroupDiscovery(ctx, collector.Store.DB, d.ID)
	if err != nil || !initial.Valid {
		t.Fatalf("initial receipt %+v %v", initial, err)
	}
	// A provider history failure does not drop the healthy receive lease.
	time.Sleep(10 * time.Millisecond)
	adapter.mu.Lock()
	adapter.historyErr = core.Fail("unavailable", "fake history failure")
	adapter.mu.Unlock()
	clock.Add(int64(11 * time.Second))
	sourceEventually(t, func() bool { clock.Add(int64(time.Second)); _, reads, _, _ := adapter.counts(); return reads >= 2 })
	lease, _ := core.ReadLease(ctx, collector.Store.DB, d.ChannelID)
	if lease.Token != first.Token || !lease.Held {
		t.Fatal("periodic history failure replaced live receiver")
	}
	// A failed discovery cannot turn the earlier group list into fresh evidence.
	time.Sleep(10 * time.Millisecond)
	adapter.mu.Lock()
	adapter.discoveryErr = core.Fail("unavailable", "fake discovery failure")
	adapter.mu.Unlock()
	_, readsBeforeFailure, _, _ := adapter.counts()
	beforeFailure, _ := core.ReadSourceGroupDiscovery(ctx, collector.Store.DB, d.ID)
	clock.Add(int64(11 * time.Second))
	sourceEventually(t, func() bool {
		clock.Add(int64(time.Second))
		r, e := core.ReadSourceGroupDiscovery(ctx, collector.Store.DB, d.ID)
		return e == nil && !r.Valid
	})
	receipt, _ := core.ReadSourceGroupDiscovery(ctx, collector.Store.DB, d.ID)
	if receipt.ObservedAt != beforeFailure.ObservedAt {
		t.Fatal("failed lookup refreshed receipt")
	}
	sourceEventually(t, func() bool { _, reads, _, _ := adapter.counts(); return reads > readsBeforeFailure })
	_, reads, starts, peak := adapter.counts()
	if starts != 1 || peak != 1 {
		t.Fatalf("failure affected receive or history: reads=%d starts=%d peak=%d", reads, starts, peak)
	}
	// A successful next discovery refreshes evidence and catches a changed scope.
	time.Sleep(10 * time.Millisecond)
	adapter.mu.Lock()
	adapter.discoveryErr = nil
	adapter.historyErr = nil
	adapter.groups = []GroupConversation{{ID: "cid:new", Name: "new group"}}
	adapter.mu.Unlock()
	clock.Add(int64(11 * time.Second))
	sourceEventually(t, func() bool {
		clock.Add(int64(time.Second))
		_, r, st, _ := adapter.counts()
		return r >= readsBeforeFailure+1 && st == 2
	})
	adapter.mu.Lock()
	scope := append([]string{}, adapter.scope...)
	peak = adapter.peak
	adapter.mu.Unlock()
	if len(scope) != 1 || scope[0] != "cid:new" || peak != 1 {
		t.Fatalf("scope shrink did not replace receiver serially: %v peak=%d", scope, peak)
	}
	receipt, err = core.ReadSourceGroupDiscovery(ctx, collector.Store.DB, d.ID)
	if err != nil || !receipt.Valid || receipt.ObservedAt == beforeFailure.ObservedAt {
		t.Fatalf("receipt not refreshed: %+v %v", receipt, err)
	}

	// An explicit ignore rule stops the remaining subscription on the next cycle.
	time.Sleep(10 * time.Millisecond)
	route, err := core.RouteFor(ctx, collector.Store.DB, d.ChannelID, "cid:new")
	if err != nil {
		t.Fatal(err)
	}
	_, err = collector.Store.Mutate(ctx, core.Request{Scope: "global", Command: "periodic.ignore"}, func(tx *core.Tx) (any, error) {
		return tx.UpdateRoute(ctx, route.ID, route.Version, core.RouteInput{Mode: "ignore"}, "exclude group")
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(11 * time.Second))
	sourceEventually(t, func() bool {
		clock.Add(int64(time.Second))
		source, e := core.ReadDataSource(ctx, collector.Store.DB, d.ID)
		adapter.mu.Lock()
		active := adapter.active
		adapter.mu.Unlock()
		return e == nil && len(source.RouteIDs) == 0 && active == 0
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("source did not stop")
	}
}
func TestDataSourceReconcileCursorSurvivesWorkerRestart(t *testing.T) {
	a := &periodicSourceAdapter{}
	collector, d := periodicFixture(t, a)
	ctx := context.Background()
	groups := []core.DataSourceGroup{}
	for i := 0; i < 21; i++ {
		groups = append(groups, core.DataSourceGroup{ID: fmt.Sprintf("cid:group%02d", i)})
	}
	_, err := collector.Store.Mutate(ctx, core.Request{Scope: "global", Command: "restart.fixture"}, func(tx *core.Tx) (any, error) {
		var e error
		d, e = tx.SyncDataSourceGroupDetails(ctx, d.ID, groups)
		return d, e
	})
	if err != nil {
		t.Fatal(err)
	}
	service := DataSourceService{Store: collector.Store, Adapter: a}
	if err = service.Reconcile(ctx, d, collector); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(ctx, collector.Store.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	d, err = core.ReadDataSource(ctx, reopened.DB, d.ID)
	if err != nil || d.ReconcileCursor != 20 {
		t.Fatalf("cursor %+v %v", d, err)
	}
	service = DataSourceService{Store: reopened, Adapter: a}
	if err = service.Reconcile(ctx, d, Collector{Store: reopened, Adapter: a}); err != nil {
		t.Fatal(err)
	}
	if a.readConversations[20] != "cid:group20" {
		t.Fatalf("restart repeated beginning: %v", a.readConversations)
	}
}

func TestDataSourceReconcileIntervalNeverHotLoops(t *testing.T) {
	for _, seconds := range []int{-1, 0, 1, 9} {
		if got := sourceReconcileInterval(core.DataSource{ReconcileSeconds: seconds}); got != 10*time.Second {
			t.Fatalf("interval %d: %v", seconds, got)
		}
	}
	if got := sourceReconcileInterval(core.DataSource{ReconcileSeconds: 300}); got != 5*time.Minute {
		t.Fatal(got)
	}
}
