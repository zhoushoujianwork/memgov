package channel

import (
	"context"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

func TestSourceRestartUsesOnlyPriorPositiveScopeDuringTransientDiscoveryFailure(t *testing.T) {
	ctx := context.Background()
	adapter := &periodicSourceAdapter{discoveryErr: core.Fail("unavailable", "fake DWS lookup timeout")}
	collector, d := periodicFixture(t, adapter)
	c, err := core.ReadChannel(ctx, collector.Store.DB, d.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = collector.Store.Mutate(ctx, core.Request{Scope: "global", Command: "fallback.seed"}, func(tx *core.Tx) (any, error) {
		workspace, e := tx.AddWorkspace(ctx, "preconfigured-scope", "")
		if e != nil {
			return nil, e
		}
		if _, e = tx.AddRoute(ctx, c.ID, core.RouteInput{ConversationID: "cid:scoped", ConversationType: "group", Workspace: workspace.ID}); e != nil {
			return nil, e
		}
		d, e = tx.SyncDataSourceGroupDetails(ctx, d.ID, []core.DataSourceGroup{{ID: "cid:scoped", Name: "Known"}, {ID: "cid:unproved", Name: "Unproved"}})
		if e != nil {
			return nil, e
		}
		if e = tx.RecordSourceGroupObservation(ctx, d.ID, d.Version, c.ConfigVersion, []core.DataSourceGroup{{ID: "cid:scoped", Name: "Known"}}, false); e != nil {
			return nil, e
		}
		return nil, tx.InvalidateSourceGroupDiscovery(ctx, d.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	original, err := core.ReadSourceGroupDiscovery(ctx, collector.Store.DB, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := collector.Store.Path
	if err = collector.Store.Close(); err != nil {
		t.Fatal(err)
	}
	var imports atomic.Int32
	// This test exercises startup discovery, not periodic rediscovery. Freeze
	// the maintenance clock so slow race-instrumented runs cannot add a cycle.
	now := time.Now()
	for run := 0; run < 2; run++ {
		store, e := core.Open(ctx, path, false)
		if e != nil {
			t.Fatal(e)
		}
		logger, e := runlog.Open(filepath.Dir(path), d.ID, io.Discard, runlog.Options{})
		if e != nil {
			t.Fatal(e)
		}
		service := DataSourceService{Store: store, Adapter: adapter, Logger: logger, Tick: time.Millisecond, Now: func() time.Time { return now }, HistoryStep: func(context.Context, string) (bool, error) { imports.Add(1); return true, nil }}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- service.Run(runCtx, d.ID) }()
		func() {
			defer func() {
				if t.Failed() {
					a, b, starts, peak := adapter.counts()
					state, _ := core.ReadDataSource(ctx, store.DB, d.ID)
					t.Log(a, b, starts, peak, state.LastErrorCode)
				}
				cancel()
				select {
				case e := <-done:
					if e != nil {
						t.Error(e)
					}
				case <-time.After(3 * time.Second):
					t.Error("source failed to stop")
				}
			}()
			// The persisted error and prior read count can already satisfy the
			// second restart. Await this restart's completed degraded-discovery
			// event before cancellation, rather than interrupting startup midway.
			deadline := time.Now().Add(15 * time.Second)
			for {
				discoveries, reads, starts, _ := adapter.counts()
				state, stateErr := core.ReadDataSource(ctx, store.DB, d.ID)
				events, logErr := runlog.Show(filepath.Dir(path), d.ID, runlog.Filter{})
				degraded := 0
				for _, event := range events {
					if event.Event == "discovery_degraded" {
						degraded++
					}
				}
				if stateErr == nil && logErr == nil && discoveries >= run+1 && reads >= run+1 && starts == run+1 && degraded == run+1 && state.LastErrorCode == "discovery_unavailable" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("restart %d did not finish degraded discovery: discoveries=%d reads=%d starts=%d events=%d state_error=%v log_error=%v", run+1, discoveries, reads, starts, degraded, stateErr, logErr)
				}
				time.Sleep(10 * time.Millisecond)
			}
			proof, e := core.ReadSourceGroupDiscovery(ctx, store.DB, d.ID)
			if e != nil || proof.Valid || proof.ObservedAt != original.ObservedAt || len(proof.Groups) != 1 {
				t.Fatalf("fallback minted new proof: %+v %v", proof, e)
			}
			adapter.mu.Lock()
			scope := append([]string{}, adapter.scope...)
			reads := append([]string{}, adapter.readConversations...)
			adapter.mu.Unlock()
			if len(scope) != 1 || scope[0] != "cid:scoped" {
				t.Fatalf("unproved route subscribed: %v", scope)
			}
			for _, id := range reads {
				if id != "cid:scoped" {
					t.Fatalf("unproved route reconciled: %s", id)
				}
			}
			persisted, e := core.ReadDataSource(ctx, store.DB, d.ID)
			if e != nil || len(persisted.RouteIDs) != 2 {
				t.Fatal("fallback rewrote configured route IDs")
			}
			if imports.Load() != 0 {
				t.Fatal("fallback started a history import job")
			}
		}()
		lease, e := core.ReadLease(ctx, store.DB, d.ChannelID)
		if e != nil || lease.Held {
			t.Fatal("restart left a receive lease")
		}
		logger.Close()
		store.Close()
	}
	events, err := runlog.Show(filepath.Dir(path), d.ID, runlog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	degraded := 0
	for _, event := range events {
		if event.Event == "discovery_degraded" {
			degraded++
		}
	}
	if degraded != 2 {
		t.Fatalf("expected independent degraded discovery per restart, got %d", degraded)
	}
}

func TestSourcePermanentDiscoveryRejectionCannotBecomeFallback(t *testing.T) {
	adapter := &periodicSourceAdapter{groups: []GroupConversation{{ID: "cid:group1", Name: "Known"}}}
	collector, d := periodicFixture(t, adapter)
	ctx := context.Background()
	service := DataSourceService{Store: collector.Store, Adapter: adapter}
	if err := service.mutate(ctx, "fallback.running", func(tx *core.Tx) (any, error) { return tx.SetDataSourceStatus(ctx, d.ID, "running") }); err != nil {
		t.Fatal(err)
	}
	d, _ = core.ReadDataSource(ctx, collector.Store.DB, d.ID)
	c, _ := core.ReadChannel(ctx, collector.Store.DB, d.ChannelID)
	if _, err := service.refreshGroups(ctx, d, c); err != nil {
		t.Fatal(err)
	}
	adapter.discoveryErr = core.Fail("denied", "fake provider revoked access")
	if _, err := service.refreshGroups(ctx, d, c); core.ErrorCode(err) != "denied" {
		t.Fatalf("expected denied: %v", err)
	}
	adapter.discoveryErr = core.Fail("unavailable", "later timeout")
	if _, err := service.refreshGroups(ctx, d, c); core.ErrorCode(err) != "unavailable" {
		t.Fatal(err)
	}
	if _, err := core.ReadSourceFallbackScope(ctx, collector.Store.DB, d.ID, time.Now()); core.ErrorCode(err) != "denied" {
		t.Fatalf("revoked proof reused after timeout: %v", err)
	}
}
