package channel

import (
	"context"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"testing"
	"time"
)

type isolatedSlowSource struct {
	periodicSourceAdapter
	fast chan struct{}
}

func (a *isolatedSlowSource) ReadWindow(ctx context.Context, _ Config, w Window) (WindowResult, error) {
	if w.ConversationID == "slow" {
		<-ctx.Done()
		return WindowResult{}, ctx.Err()
	}
	select {
	case a.fast <- struct{}{}:
	default:
	}
	return WindowResult{Complete: true}, nil
}

func TestReconcileSlowConversationDoesNotBlockOthers(t *testing.T) {
	adapter := &isolatedSlowSource{fast: make(chan struct{}, 1)}
	collector, d := periodicFixture(t, &adapter.periodicSourceAdapter)
	collector.Adapter = adapter
	ctx := context.Background()
	_, err := collector.Store.Mutate(ctx, core.Request{Scope: "global", Command: "parallel.fixture"}, func(tx *core.Tx) (any, error) {
		var e error
		d, e = tx.SyncDataSourceGroups(ctx, d.ID, []string{"slow", "fast"})
		return d, e
	})
	if err != nil {
		t.Fatal(err)
	}
	service := DataSourceService{Store: collector.Store, Adapter: adapter}
	bounded, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Reconcile(bounded, d, collector) }()
	select {
	case <-adapter.fast:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("slow route blocked another conversation")
	}
	if err = <-done; err == nil {
		t.Fatal("timeout hidden")
	}
	var deferred, gaps int
	if err = collector.Store.DB.QueryRow("SELECT count(*) FROM source_route_backoff WHERE source_id=?", d.ID).Scan(&deferred); err != nil || deferred != 1 {
		t.Fatalf("missing backoff: %d %v", deferred, err)
	}
	if err = collector.Store.DB.QueryRow("SELECT count(*) FROM coverage_windows WHERE complete=0").Scan(&gaps); err != nil || gaps == 0 {
		t.Fatalf("timeout has no coverage gap: %d %v", gaps, err)
	}
}
