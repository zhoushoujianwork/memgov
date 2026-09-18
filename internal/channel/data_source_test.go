package channel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestDataSourceLifecycleRequiresNoSendAndReleasesLeaseOnPause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := &blockingAdapter{started: make(chan Config, 10)}
	collector, c := testCollector(t, adapter)
	s := collector.Store
	var source core.DataSource
	_, err := s.Mutate(ctx, core.Request{Scope: "global", Command: "source.fixture"}, func(tx *core.Tx) (any, error) {
		if _, e := tx.SetChannelCapabilities(ctx, c.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); e != nil {
			return nil, e
		}
		if _, e := tx.AddRoute(ctx, c.ID, core.RouteInput{ConversationID: "cid:owner", ConversationType: "direct", Workspace: "global", Mode: "notify", SendPolicy: "dispatch_only"}); e != nil {
			return nil, e
		}
		var e error
		source, e = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "work", Channel: c.ID, Workspace: "global"})
		if e != nil {
			return nil, e
		}
		// The lifecycle fixture represents a source whose group was discovered.
		source, e = tx.SyncDataSourceGroups(ctx, source.ID, []string{c.Routes[0].ConversationID})
		return source, e
	})
	if err != nil {
		t.Fatal(err)
	}
	var historySteps atomic.Int32
	historyFailed := make(chan struct{}, 1)
	service := DataSourceService{Store: s, Adapter: adapter, Tick: 5 * time.Millisecond,
		HistoryStep: func(context.Context, string) (bool, error) {
			if historySteps.Add(1) == 1 {
				historyFailed <- struct{}{}
				return false, core.Fail("unavailable", "fake history page failed")
			}
			return false, nil
		}}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx, source.ID) }()
	waitStart := func() {
		t.Helper()
		select {
		case cfg := <-adapter.started:
			if len(cfg.DirectConversations) != 0 || len(cfg.Conversations) != 1 {
				t.Fatalf("source must subscribe only to group acquisition: %+v", cfg)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("receiver did not start")
		}
	}
	waitStart()
	select {
	case <-historyFailed:
	case <-time.After(3 * time.Second):
		t.Fatal("history failure was not exercised")
	}
	leaseAfterHistory, e := core.ReadLease(ctx, s.DB, c.ID)
	if e != nil || !leaseAfterHistory.Held {
		t.Fatalf("history failure dropped live receiver: %+v %v", leaseAfterHistory, e)
	}
	select {
	case <-adapter.started:
		t.Fatal("history failure restarted live receiver")
	default:
	}
	set := func(status string) {
		t.Helper()
		_, e := s.Mutate(ctx, core.Request{Scope: "global", Command: "source.control"}, func(tx *core.Tx) (any, error) { return tx.SetDataSourceStatus(ctx, source.ID, status) })
		if e != nil {
			t.Fatal(e)
		}
	}
	// A second foreground source must not replace the active channel receiver.
	if e := service.Run(ctx, source.ID); core.ErrorCode(e) != "conflict" {
		t.Fatalf("duplicate receiver: %v", e)
	}
	set("paused")
	deadline := time.Now().Add(3 * time.Second)
	for {
		l, e := core.ReadLease(ctx, s.DB, c.ID)
		if e != nil {
			t.Fatal(e)
		}
		if !l.Held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pause retained receive lease")
		}
		time.Sleep(5 * time.Millisecond)
	}
	paused, e := core.ReadDataSource(ctx, s.DB, source.ID)
	if e != nil || paused.Status != "paused" {
		t.Fatalf("paused state: %+v %v", paused, e)
	}
	set("running")
	waitStart()
	set("stopped")
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not exit")
	}
	l, e := core.ReadLease(ctx, s.DB, c.ID)
	if e != nil || l.Held {
		t.Fatalf("stop lease: %+v %v", l, e)
	}
}

func TestManagedSourceCancellationPreservesPausedControl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := &blockingAdapter{started: make(chan Config, 10)}
	collector, c := testCollector(t, adapter)
	s := collector.Store
	var source core.DataSource
	_, err := s.Mutate(ctx, core.Request{Scope: "global", Command: "test.managed.source"}, func(tx *core.Tx) (any, error) {
		if _, e := tx.SetChannelCapabilities(ctx, c.ID, core.Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); e != nil {
			return nil, e
		}
		var e error
		source, e = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "work", Channel: c.ID, Workspace: "global"})
		if e != nil {
			return nil, e
		}
		return tx.SyncDataSourceGroups(ctx, source.ID, []string{c.Routes[0].ConversationID})
	})
	if err != nil {
		t.Fatal(err)
	}
	service := DataSourceService{Store: s, Adapter: adapter, ExternalLifecycle: true, Tick: 5 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx, source.ID) }()
	select {
	case <-adapter.started:
	case <-time.After(3 * time.Second):
		t.Fatal("receiver did not start")
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "test.managed.pause"}, func(tx *core.Tx) (any, error) { return tx.SetDataSourceStatus(ctx, source.ID, "paused") })
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not exit")
	}
	saved, err := core.ReadDataSource(context.Background(), s.DB, source.ID)
	if err != nil || saved.Status != "paused" {
		t.Fatal(saved, err)
	}
	lease, err := core.ReadLease(context.Background(), s.DB, c.ID)
	if err != nil || lease.Held {
		t.Fatal(lease, err)
	}
}
